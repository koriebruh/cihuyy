# ─── Variables ────────────────────────────────────────────────────────────────
BINARY_NAME := matching-engine
BUILD_DIR   := ./bin
MAIN_PKG    := ./cmd/server
COVER_FILE  := coverage.out
COVER_HTML  := coverage.html

.DEFAULT_GOAL := help

# ─── Targets ──────────────────────────────────────────────────────────────────
.PHONY: build run test test-cover lint docker-build docker-up docker-down tidy clean help

## build: Compile the binary to ./bin/matching-engine
build:
	@mkdir -p $(BUILD_DIR)
	go build -ldflags="-w -s" -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PKG)
	@echo "✓ Binary: $(BUILD_DIR)/$(BINARY_NAME)"

## run: Run the server directly with go run
run:
	go run $(MAIN_PKG)

## test: Run all tests with the race detector
test:
	go test -race ./...

## test-cover: Run tests, generate coverage profile and HTML report
test-cover:
	go test -race -coverprofile=$(COVER_FILE) ./...
	go tool cover -html=$(COVER_FILE) -o $(COVER_HTML)
	@echo "✓ Coverage report: $(COVER_HTML)"

## lint: Run golangci-lint against the entire module
lint:
	golangci-lint run ./...

## docker-build: Build the Docker image
docker-build:
	docker build -t $(BINARY_NAME):latest .

## docker-up: Start all services with Docker Compose (detached)
docker-up:
	docker-compose up -d

## docker-down: Stop and remove all Docker Compose services
docker-down:
	docker-compose down

## tidy: Tidy and verify Go module dependencies
tidy:
	go mod tidy
	go mod verify

## clean: Remove compiled binary and coverage artifacts
clean:
	rm -rf $(BUILD_DIR) $(COVER_FILE) $(COVER_HTML)
	@echo "✓ Cleaned"

## help: Print this help message
help:
	@echo "Usage: make <target>"
	@echo ""
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / \
		{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
