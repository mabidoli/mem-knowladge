.PHONY: build run test clean docker-up docker-down install-model

# Build the UMS server binary
build:
	go build -o bin/ums ./cmd/ums

# Run the server locally
run: build
	./bin/ums

# Run tests
test:
	go test -v ./...

# Clean build artifacts
clean:
	rm -rf bin/

# Start infrastructure (Neo4j + Ollama)
docker-up:
	docker compose up -d neo4j ollama ollama-init

# Start with monitoring stack
docker-up-full:
	docker compose --profile monitoring up -d

# Stop all containers
docker-down:
	docker compose --profile monitoring down

# Pull the embedding model manually
install-model:
	ollama pull nomic-embed-text

# Initialize Neo4j schema (run after docker-up)
init-db:
	@echo "Waiting for Neo4j..."
	@sleep 10
	@echo "Neo4j should be ready. Schema will be created on first connection."

# Development: run with hot reload (requires air)
dev:
	air

# Lint code
lint:
	golangci-lint run ./...

# Format code
fmt:
	go fmt ./...
	goimports -w .
