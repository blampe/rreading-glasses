SHELL = /bin/bash
PROJECT_ROOT = $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

.PHONY: all
all: build lint test

.PHONY: build
build: go.mod $(wildcard *.go) $(wildcard */*.go)
	go build -o $(PROJECT_ROOT)/bin/rreading-glasses

.PHONY: lint
lint:
	go tool golangci-lint run --fix --timeout 10m

.PHONY: test
test:
	go test -v -count=1 -coverpkg=./... ./...

.PHONY: release
release:
	docker build -f Dockerfile \
		--builder multiarch \
		--platform linux/amd64,linux/arm64 \
		--tag docker.io/blampe/rreading-glasses:hardcover \
		--push \
		.
