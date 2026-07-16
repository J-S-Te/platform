GO ?= go
GO_PACKAGES := ./cmd/... ./internal/... ./migrations

.PHONY: fmt test vet run-api migrate tidy

fmt: ## Format backend Go source files.
	$(GO) fmt $(GO_PACKAGES)

test: ## Run backend unit tests.
	$(GO) test $(GO_PACKAGES)

vet: ## Run static analysis for backend Go packages.
	$(GO) vet $(GO_PACKAGES)

run-api: ## Start the HTTP API using the project-root .env file.
	$(GO) run ./cmd/api

migrate: ## Apply embedded MySQL schema migrations using the project-root .env file.
	$(GO) run ./cmd/migrate

tidy: ## Synchronize backend module dependencies.
	$(GO) mod tidy
