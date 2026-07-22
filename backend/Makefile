GO ?= go
GO_PACKAGES := ./cmd/... ./internal/... ./migrations

.PHONY: fmt vet run-api run-worker migrate tidy generate-dev-jwt-keys

fmt: ## Format backend Go source files.
	$(GO) fmt $(GO_PACKAGES)

vet: ## Run static analysis for backend Go packages.
	$(GO) vet $(GO_PACKAGES)

run-api: ## Start the HTTP API using the repository-root .env (or ENV_FILE override).
	$(GO) run ./cmd/api

run-worker: ## Start the asynchronous audit-export worker using the repository-root .env.
	$(GO) run ./cmd/worker

migrate: ## Apply embedded MySQL schema migrations using the repository-root .env (or ENV_FILE override).
	$(GO) run ./cmd/migrate

tidy: ## Synchronize backend module dependencies.
	$(GO) mod tidy

generate-dev-jwt-keys: ## Generate a local Ed25519 JWT key pair under ../data/keys.
	@set -eu; \
	key_dir=../data/keys; \
	if test -e "$$key_dir"; then \
		echo "$$key_dir already exists; refusing to overwrite it"; \
		exit 1; \
	fi; \
	umask 077; \
	mkdir -p ../data; \
	tmp_dir=$$(mktemp -d ../data/.jwt-keys.XXXXXX); \
	private_key="$$tmp_dir/jwt-ed25519-private.pem"; \
	public_key="$$tmp_dir/jwt-ed25519-public.pem"; \
	trap 'rm -f "$$private_key" "$$public_key"; rmdir "$$tmp_dir" 2>/dev/null || true' EXIT HUP INT TERM; \
	openssl genpkey -algorithm ED25519 -out "$$private_key"; \
	openssl pkey -in "$$private_key" -pubout -out "$$public_key"; \
	chmod 600 "$$private_key"; \
	mv "$$tmp_dir" "$$key_dir"; \
	trap - EXIT HUP INT TERM
