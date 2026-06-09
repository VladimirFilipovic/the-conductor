BINARY    := conductor
BUILD_DIR := ./build
PREFIX    ?= /usr/local
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X conductor/cmd.version=$(VERSION)

.PHONY: build run install uninstall fmt vet test tidy clean

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

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
