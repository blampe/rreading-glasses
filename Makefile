SHALL = /bin/bash
PROJECT_ROOT = $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

export GOBIN = $(PROJECT_ROOT)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: all
all: build lint test

.PHONY: build
build: go.mod $(wildcard *.go) $(wildcard */*.go)
	go build -o $(PROJECT_ROOT)/bin/rreading-glasses

.PHONY: lint
lint:
	go tool golangci-lint run --fix

.PHONY: test
test:
	go test -v -count=1 -coverpkg=./... ./...

.PHONY:
serve: build
	rreading-glasses
