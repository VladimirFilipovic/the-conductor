BINARY    := conductor
BUILD_DIR := ./build
PREFIX    ?= /usr/local
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X conductor/cmd.version=$(VERSION)

.PHONY: build run install uninstall fmt vet test tidy clean migrate migrate-down migrate-status migrate-fresh sqlc db-up db-down

# Local dev database (docker-compose). Override these in your environment to
# point elsewhere (?= keeps your value); `export` makes them visible to every
# recipe's shell, so `make run ARGS="add ..."`, `make migrate`, and `make test`
# all see them without you setting anything.
CONDUCTOR_DATABASE_URL ?= postgres://conductor:conductor@localhost:5432/conductor?sslmode=disable
CONDUCTOR_TEST_DSN     ?= postgres://conductor:conductor@localhost:5432/conductor?sslmode=disable
export CONDUCTOR_DATABASE_URL
export CONDUCTOR_TEST_DSN

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

# Wipe the local Postgres volume and re-apply every migration from scratch.
# Use after editing a migration in place (goose tracks version numbers, so an
# edited-but-already-applied migration never re-runs). --wait blocks until the
# healthcheck passes so goose doesn't race the container's startup.
migrate-fresh:
	docker compose down -v
	docker compose up -d --wait
	$(GOOSE) up

# Regenerate type-safe query code from queries/ against the schema in
# migrations/ (install once: `brew install sqlc`).
sqlc:
	sqlc generate

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
