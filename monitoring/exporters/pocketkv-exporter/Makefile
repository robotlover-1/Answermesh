BIN ?= bin/pocketkv-exporter
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
TARGET ?= 127.0.0.1:5160
LISTEN ?= 127.0.0.1:9401
ROLE ?= auto

.PHONY: build test run clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) .

test:
	go test -race ./...

run: build
	./$(BIN) --target=$(TARGET) --listen=$(LISTEN) --role=$(ROLE)

clean:
	rm -rf bin
