.PHONY: build test test-coverage run lint sim dashboard clean deps bench

# Spec targets (docs/spec/01-setup.md): build, test, run, lint, sim.

build:
	go build ./...

test:
	go test ./... -v -race

test-coverage:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

run:
	go run ./cmd/server

lint:
	go vet ./...

sim:
	go run ./cmd/simulator

dashboard:
	go run ./cmd/dashboard

clean:
	rm -rf bin coverage.out coverage.html

deps:
	go mod download
	go mod tidy

bench:
	go test -bench=. -benchmem ./test/benchmark/...
