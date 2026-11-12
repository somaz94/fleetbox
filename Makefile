.PHONY: build test cover fmt vet lint check-config clean help

APP_NAME=fleetbox
MODULE=github.com/somaz94/fleetbox
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-s -w -X $(MODULE)/cmd/cli.Version=$(VERSION) -X $(MODULE)/cmd/cli.GitCommit=$(GIT_COMMIT) -X $(MODULE)/cmd/cli.BuildDate=$(BUILD_DATE)"
# Two workers keep a laptop responsive; CI may raise it.
TEST_P ?= 2

build: ## Build the binary into bin/
	CGO_ENABLED=0 go build $(LDFLAGS) -o bin/$(APP_NAME) ./cmd/

test: ## Run unit tests with the race detector and coverage
	go test -p $(TEST_P) -race -cover ./...

cover: ## Write coverage.out and print per-function coverage
	go test -p $(TEST_P) -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

fmt: ## Format code
	gofmt -w .

vet: ## Run go vet
	go vet ./...

lint: vet ## Check formatting and vet
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }

check-config: build ## Validate a configuration file (usage: make check-config CONFIG=path)
	@test -n "$(CONFIG)" || { echo "usage: make check-config CONFIG=path"; exit 1; }
	./bin/$(APP_NAME) check --config $(CONFIG)

clean: ## Remove build artifacts and coverage files
	rm -rf bin/ coverage.out

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
