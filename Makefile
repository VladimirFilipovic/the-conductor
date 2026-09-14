BINARY    := conductor
BUILD_DIR := ./build
PREFIX    ?= /usr/local
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X conductor/cmd.version=$(VERSION)

.PHONY: build run install uninstall fmt vet lint test tidy clean migrate migrate-down migrate-status migrate-fresh sqlc db-up db-down seed stack-up stack-fresh stack-down stack-logs ui-dev

GOLANGCI_LINT_VERSION ?= v2.11.4

# Local dev database (docker-compose). Override these in your environment to
# point elsewhere (?= keeps your value); `export` makes them visible to every
# recipe's shell, so `make run ARGS="add ..."`, `make migrate`, and `make test`
# all see them without you setting anything.
CONDUCTOR_DATABASE_URL ?= postgres://conductor:conductor@localhost:5432/conductor?sslmode=disable
CONDUCTOR_TEST_DSN     ?= postgres://conductor:conductor@localhost:5432/conductor?sslmode=disable
LOG_LEVEL              ?= DEBUG
export CONDUCTOR_DATABASE_URL
export CONDUCTOR_TEST_DSN
export LOG_LEVEL

build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) .

run:
	go run . $(ARGS)

install: build
	install -m 0755 $(BUILD_DIR)/$(BINARY) $(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

fmt:
	go fmt ./...

vet:
	go vet ./...

# Run golangci-lint (install once:
# `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)`).
# Pass `make lint ARGS="--fix"` to auto-fix issues where supported.
lint:
	golangci-lint run $(ARGS)

test:
	go test ./...

# Start/stop the local Postgres container.
db-up:
	docker compose up -d

db-down:
	docker compose down

# Migrations are run by the goose CLI (install once:
# `go install github.com/pressly/goose/v3/cmd/goose@latest`) against the
# migrations dir, using $(CONDUCTOR_DATABASE_URL).
MIGRATIONS_DIR := db/migrations
GOOSE          := goose -dir $(MIGRATIONS_DIR) postgres "$(CONDUCTOR_DATABASE_URL)"

migrate:
	$(GOOSE) up

migrate-down:
	$(GOOSE) down

migrate-status:
	$(GOOSE) status

# Rebuild schema + fleet from scratch. Works on the database only: the Postgres
# container and its volume stay put, so this is the cheap one to reach for while
# developing against `go run`. psql runs inside the container the way `seed`
# does, so you don't need it installed locally.
# DROP SCHEMA rather than `goose reset`: it owes nothing to the Down half of
# every migration being written and correct, and it takes goose's own version
# table with it, so `up` really does replay from zero.
migrate-fresh:
	docker compose up -d --wait
	docker compose exec -T postgres psql -U conductor -d conductor -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
	$(GOOSE) up
	$(MAKE) seed

# Load dev fixtures into the local Postgres. Piped into the container's own psql
# (like db-up/db-down, this targets the docker-compose database) so you don't
# need psql installed locally. Idempotent — safe to re-run. Run after `migrate`.
SEED_FILE := db/seeds/hosts.sql
seed:
	docker compose exec -T postgres psql -U conductor -d conductor < $(SEED_FILE)

# Regenerate type-safe query code from queries/ against the schema in
# migrations/ (install once: `brew install sqlc`).
sqlc:
	sqlc generate

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)

# --- full stack (postgres + engine + apiserver + agentsim + chaos-ui) --------------------
# The engine container migrates and seeds on startup (see docker/engine-
# entrypoint.sh), so neither target needs a separate `migrate`/`seed` step.
# Nothing in that path ever drops: a wipe only happens where you asked for one,
# and `stack-fresh` is that ask — it removes the database volume, so the stack
# comes back up on a schema built from scratch. Same deal as `migrate-fresh`
# for the `go run` workflow.

stack-up:
	docker compose --profile stack up --build -d

stack-fresh:
	docker compose --profile stack down -v
	docker compose --profile stack up --build -d

stack-down:
	docker compose --profile stack down

stack-logs:
	docker compose --profile stack logs -f engine apiserver agentsim chaos-ui

# Run the Next.js dev server locally against a running apiserver. Reads
# chaos-ui/.env.local if present; defaults target localhost:7080 (control plane)
# and localhost:7780 (agentsim).
ui-dev:
	cd chaos-ui && npm install && npm run dev
