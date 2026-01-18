.PHONY: build test run lint clean

BINARY_NAME=batching-engine
BUILD_DIR=bin

build:
	go build -o $(BUILD_DIR)/server ./cmd/server
	go build -o $(BUILD_DIR)/simulator ./cmd/simulator

test:
	go test -v -race ./...

test-coverage:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

run:
	go run ./cmd/server

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

deps:
	go mod download
	go mod tidy

bench:
	go test -bench=. -benchmem ./test/benchmark/...
