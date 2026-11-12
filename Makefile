.PHONY: build test cover generate fmt vet lint check-config docker-build helm-lint clean help

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

cover: ## Print the coverage total the 90% rule applies to
	go test -p $(TEST_P) -coverprofile=coverage.out ./...
	@# Excluded: generated code, the main entry point, and the test-support packages (providertest, oidctest).
	grep -vE '\.gen\.go:|/cmd/main\.go:|/providertest/|/oidctest/' coverage.out > coverage.hand.out
	go tool cover -func=coverage.hand.out | tail -1

generate: ## Regenerate internal/api/api.gen.go from api/openapi.yaml
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

fmt: ## Format code
	gofmt -w .

vet: ## Run go vet
	go vet ./...

lint: vet ## Check formatting and vet
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }

check-config: build ## Validate a configuration file (usage: make check-config CONFIG=path)
	@test -n "$(CONFIG)" || { echo "usage: make check-config CONFIG=path"; exit 1; }
	./bin/$(APP_NAME) check --config $(CONFIG)

docker-build: ## Build the container image for the local platform as fleetbox:dev
	docker build --build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE) -t $(APP_NAME):dev .

helm-lint: ## Lint the chart and render it with the CI values
	helm lint helm/$(APP_NAME) -f helm/$(APP_NAME)/ci/default-values.yaml
	helm template ci helm/$(APP_NAME) -f helm/$(APP_NAME)/ci/default-values.yaml > /dev/null

clean: ## Remove build artifacts and coverage files
	rm -rf bin/ coverage.out coverage.hand.out

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
