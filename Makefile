SHELL = /bin/bash
PROJECT_ROOT = $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

.PHONY: all
all: build lint test

.PHONY: generate
generate: go.mod $(wildcard *.go) $(wildcard */*.go)
	go generate ./...

.PHONY: build-hc
build-hc: generate go.mod $(wildcard *.go) $(wildcard */*.go)
	go build -o $(PROJECT_ROOT)/bin/rghc ./cmd/rghc/...

.PHONY: build-gr
build-gr: generate go.mod $(wildcard *.go) $(wildcard */*.go)
	go build -o $(PROJECT_ROOT)/bin/rggr ./cmd/rggr/...

.PHONY: serve-gr
serve-gr:
	go run ./cmd/rggr/main.go serve --verbose --upstream=www.goodreads.com --port 8080

.PHONY: serve-hc
serve-gc:
	go run ./cmd/rghc/main.go serve --verbose --port 8080 --hardcover-auth "Bearer $(HARDCOVER_API_KEY)"

.PHONY: build
build: build-hc build-gr

.PHONY: lint
lint:
	golangci-lint run --fix --timeout 10m

.PHONY: test
test:
	go test -v -count=1 -race -coverpkg=./... -covermode=atomic -coverprofile=coverage.txt ./...

.PHONY: release-hc
release-hc:
	docker build -f Dockerfile \
		--builder multiarch \
		--platform linux/amd64,linux/arm64 \
		--tag docker.io/blampe/rreading-glasses:hardcover \
		--build-arg RGPATH=./cmd/rghc \
		--push \
		.

.PHONY: release-gr
release-gr:
	docker build -f Dockerfile \
		--builder multiarch \
		--platform linux/amd64,linux/arm64 \
		--tag docker.io/blampe/rreading-glasses:latest \
		--build-arg RGPATH=./cmd/rggr \
		--push \
		.

.PHONY: release
release: release-hc release-gr
