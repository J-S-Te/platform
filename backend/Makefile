GO ?= go
GO_PACKAGES := ./cmd/... ./backend/internal/... ./backend/migrations

.PHONY: fmt test vet run-api migrate tidy generate-dev-jwt-keys

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

generate-dev-jwt-keys: ## Generate a local Ed25519 JWT key pair under data/keys.
	@test ! -e data/keys/jwt-ed25519-private.pem || (echo "data/keys/jwt-ed25519-private.pem already exists; refusing to overwrite it"; exit 1)
	@mkdir -p data/keys
	openssl genpkey -algorithm ED25519 -out data/keys/jwt-ed25519-private.pem
	openssl pkey -in data/keys/jwt-ed25519-private.pem -pubout -out data/keys/jwt-ed25519-public.pem
	chmod 600 data/keys/jwt-ed25519-private.pem
