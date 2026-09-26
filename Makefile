SHELL := /bin/bash

# Local settings (DATABASE_URL, REDIS_ADDR, ...) are read from .env and exported to every recipe.
-include .env
export

BIN_DIR      ?= bin
IMAGE        ?= cinema-api:dev
PG_CONTAINER ?= local-postgres
DB_NAME      ?= cinema

# The race detector needs cgo and a C compiler. Without gcc the tests still run, just without -race.
RACE := $(if $(shell command -v gcc 2>/dev/null),-race,)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build every binary in ./cmd into ./bin
	CGO_ENABLED=0 go build -trimpath -o $(BIN_DIR)/ ./cmd/...

.PHONY: run-api
run-api: ## Run the HTTP API with settings from .env
	go run ./cmd/api

.PHONY: migrate-up
migrate-up: ## Apply all pending database migrations
	go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent database migration
	go run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: ## Show which migrations are applied
	go run ./cmd/migrate status

.PHONY: seed
seed: ## Seed demo movies, halls, and a week of showtimes (only into an empty catalog)
	go run ./cmd/seed

.PHONY: seed-reset
seed-reset: ## Delete the catalog, bookings, and payments, then seed again
	go run ./cmd/seed -reset

.PHONY: test
test: ## Run unit tests (with -race when gcc is available)
	@[ -n "$(RACE)" ] || echo "warning: gcc not found, running tests WITHOUT the race detector" >&2
	CGO_ENABLED=$(if $(RACE),1,0) go test $(RACE) -count=1 -shuffle=on ./...

.PHONY: test-integration
test-integration: ## Run integration tests against throwaway containers (needs Docker)
	CGO_ENABLED=$(if $(RACE),1,0) go test $(RACE) -tags=integration -count=1 ./test/integration/...

.PHONY: cover
cover: ## Run unit tests and print total coverage
	CGO_ENABLED=$(if $(RACE),1,0) go test $(RACE) -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format code (gofmt + goimports)
	golangci-lint fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: check
check: vet lint test ## Run vet, lint, and tests

.PHONY: db-create
db-create: ## Create the database in the shared PostgreSQL container if it does not exist
	@docker exec $(PG_CONTAINER) psql -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$(DB_NAME)'" | grep -q 1 \
		&& echo "database $(DB_NAME) already exists" \
		|| docker exec $(PG_CONTAINER) psql -U postgres -c "CREATE DATABASE $(DB_NAME)"

.PHONY: docker-build
docker-build: ## Build the application image
	docker build -t $(IMAGE) .

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf $(BIN_DIR) coverage.out
