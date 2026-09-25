# codex-telegram-bot
#
# Everything here is a thin wrapper over `go`; the Makefile exists so the common
# commands are discoverable, not to hide anything.

BINARY  := codex-telegram-bot
PKG     := ./cmd/bot
PREFIX  ?= $(HOME)/.local

.PHONY: all build install test race vet fmt fmt-check tidy clean check

all: check build

## build: compile the bot into ./bin
build:
	go build -o bin/$(BINARY) $(PKG)

## install: build and copy into $(PREFIX)/bin
install:
	install -d -m 0755 $(PREFIX)/bin
	go build -o $(PREFIX)/bin/$(BINARY) $(PKG)

## test: run the suite (no network, no real account)
test:
	go test ./...

## race: run the suite under the race detector
race:
	go test -race -count=1 ./...

## vet: go vet
vet:
	go vet ./...

## fmt: rewrite sources with gofmt
fmt:
	gofmt -w .

## fmt-check: fail if anything is unformatted
fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

## tidy: tidy go.mod and go.sum
tidy:
	go mod tidy

## check: everything that must pass before a commit
check: fmt-check vet test

## clean: remove build output
clean:
	rm -rf bin
