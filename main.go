package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"gopkg.in/yaml.v2"
)

// Server represents an Ollama server configuration
type Server struct {
	URL           string `yaml:"url"`
	Model         string `yaml:"model"`
	ContainerName string `yaml:"container_name"`
}

// Config holds the application configuration
type Config struct {
	Servers []Server `yaml:"servers"`
	Timeout int      `yaml:"timeout"` // in seconds
}

// CrashEvent represents a crash event stored in MongoDB
type CrashEvent struct {
	Timestamp time.Time `bson:"timestamp"`
	URL       string    `bson:"url"`
	Model     string    `bson:"model"`
	CrashType string    `bson:"crash_type"` // e.g., "modelTimeouted", "ollamaTimeouted"
}

// RestartEvent represents a container restart attempt stored in MongoDB
type RestartEvent struct {
	Timestamp     time.Time `bson:"timestamp"`
	ContainerName string    `bson:"container_name"`
	URL           string    `bson:"url"`
	Model         string    `bson:"model"`
	Status        string    `bson:"status"` // "success" or "fail"
	ErrorMessage  string    `bson:"error_message,omitempty"` // Error message if status is "fail"
}

// loadConfig reads and parses the YAML configuration file
func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// connectMongoDB establishes a connection to MongoDB
func connectMongoDB(uri string) (*mongo.Client, error) {
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	err = client.Ping(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// checkServer sends a request to an Ollama server, logs a crash event, and attempts a container restart if it fails
func checkServer(server Server, timeout int, crashCollection, restartCollection *mongo.Collection) {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: time.Duration(timeout) * time.Second,
	}
	client := &http.Client{
		Transport: transport,
	}

	payload := struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}{
		Model: server.Model,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			{
				Role:    "user",
				Content: "create a json response that status is true;just give me json dont explain somthing",
			},
		},
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal payload for %s: %v", server.URL, err)
		return
	}

	req, err := http.NewRequest("POST", server.URL, bytes.NewReader(payloadBytes))
	if err != nil {
		log.Printf("Failed to create request for %s: %v", server.URL, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout+5)*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("err my abo: %v\n", err)
		crashType := "ollamaTimeouted"
		if err == context.DeadlineExceeded {
			crashType = "modelTimeouted"
		}

		// Log crash event
		event := CrashEvent{
			Timestamp: time.Now(),
			URL:       server.URL,
			Model:     server.Model,
			CrashType: crashType,
		}
		_, insertErr := crashCollection.InsertOne(context.Background(), event)
		if insertErr != nil {
			log.Printf("Failed to insert crash event for %s: %v", server.URL, insertErr)
		} else {
			log.Printf("Logged crash event for %s (model: %s, type: %s)", server.URL, server.Model, crashType)
		}

		// Attempt container restart and log it
		if server.ContainerName != "" {
			restartEvent := RestartEvent{
				Timestamp:     time.Now(),
				ContainerName: server.ContainerName,
				URL:           server.URL,
				Model:         server.Model,
			}
			cmd := exec.Command("docker", "restart", server.ContainerName)
			if err := cmd.Run(); err != nil {
				log.Printf("Failed to restart container %s for server %s: %v", server.ContainerName, server.URL, err)
				restartEvent.Status = "fail"
				restartEvent.ErrorMessage = err.Error()
			} else {
				log.Printf("Successfully restarted container %s for server %s", server.ContainerName, server.URL)
				restartEvent.Status = "success"
			}
			_, insertErr := restartCollection.InsertOne(context.Background(), restartEvent)
			if insertErr != nil {
				log.Printf("Failed to insert restart event for container %s: %v", server.ContainerName, insertErr)
			} else {
				log.Printf("Logged restart event for container %s (status: %s)", server.ContainerName, restartEvent.Status)
			}
		} else {
			log.Printf("No container_name specified for server %s, skipping restart", server.URL)
		}

		log.Printf("Error checking server %s (type: %s): %v", server.URL, crashType, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Server %s returned non-200 status: %s", server.URL, resp.Status)
	}
}

// startScheduler initiates the cron job to check servers every 30 minutes
func startScheduler(config *Config, crashCollection, restartCollection *mongo.Collection) {
	for _, server := range config.Servers {
		go checkServer(server, config.Timeout, crashCollection, restartCollection)
	}

	c := cron.New()
	_, err := c.AddFunc("@every 1800s", func() {
		for _, server := range config.Servers {
			go checkServer(server, config.Timeout, crashCollection, restartCollection)
		}
	})
	if err != nil {
		log.Fatalf("Failed to schedule job: %v", err)
	}
	c.Start()
	log.Println("Scheduler started, checking servers every 30 minutes")
}

// fetchEvents is a helper to query events from a MongoDB collection
func fetchEvents(w http.ResponseWriter, r *http.Request, collection *mongo.Collection, entityType string) {
	limitStr := r.URL.Query().Get("limit")
	sortStr := r.URL.Query().Get("sort")

	limit := int64(10)
	sortOrder := -1 // descending (newest first)
	if limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = int64(parsedLimit)
		}
	}
	if sortStr == "asc" {
		sortOrder = 1 // ascending (oldest first)
	}

	findOptions := options.Find()
	findOptions.SetSort(bson.D{{"timestamp", sortOrder}})
	findOptions.SetLimit(limit)

	cursor, err := collection.Find(context.Background(), bson.M{}, findOptions)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to query %s", entityType), http.StatusInternalServerError)
		log.Printf("Database query error for %s: %v", entityType, err)
		return
	}
	defer cursor.Close(context.Background())

	var results []bson.M
	if err = cursor.All(context.Background(), &results); err != nil {
		http.Error(w, fmt.Sprintf("Failed to decode %s", entityType), http.StatusInternalServerError)
		log.Printf("Cursor decode error for %s: %v", entityType, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(results); err != nil {
		log.Printf("Failed to encode %s response: %v", entityType, err)
	}
}

func main() {
	// Load configuration from /usr/share/llm-watcher/config.yaml
	config, err := loadConfig("/usr/share/llm-watcher/config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Get MongoDB URL from environment variable or default to container hostname
	mongoURL := os.Getenv("MONGO_URL")
	if mongoURL == "" {
		mongoURL = "mongodb://mongo:27017"
	}

	// Connect to MongoDB
	mongoClient, err := connectMongoDB(mongoURL)
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	db := mongoClient.Database("ollama_monitor")
	crashCollection := db.Collection("crash_events")
	restartCollection := db.Collection("restart_events")

	// Start the scheduler in a goroutine
	go startScheduler(config, crashCollection, restartCollection)

	// Set up REST API
	http.HandleFunc("/crashes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			fetchEvents(w, r, crashCollection, "crash events")

		case http.MethodDelete:
			result, err := crashCollection.DeleteMany(context.Background(), bson.M{})
			if err != nil {
				http.Error(w, "Failed to delete crash events", http.StatusInternalServerError)
				log.Printf("Delete error: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"message":      "All crash events deleted",
				"deletedCount": result.DeletedCount,
			})
			log.Printf("Deleted %d crash events", result.DeletedCount)

		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	http.HandleFunc("/restarts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fetchEvents(w, r, restartCollection, "restart events")
	})

	log.Println("Starting REST API server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}