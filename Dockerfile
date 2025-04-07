# Build stage
FROM golang:1.20-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o llm-watcher

# Final stage
FROM alpine:3.18
WORKDIR /usr/share/llm-watcher
COPY --from=builder /app/llm-watcher .
EXPOSE 8080
CMD ["./llm-watcher"]
