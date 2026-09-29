# mockmint build targets. Requires Go (see go.mod) and, for lint, golangci-lint v2.

BINARY   := mockmint
PKG      := ./cmd/mockmint
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
IMAGE    ?= mockmint:$(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64

export CGO_ENABLED ?= 0

.PHONY: all build test race integration lint vet fmt bench docker docker-multiarch clean

all: lint test build

build: ## Static, stripped binary in ./bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

test:
	go test ./...

race: ## Race detector needs cgo
	CGO_ENABLED=1 go test -race ./...

integration: ## RabbitMQ integration tests; needs Docker or Podman (Podman: TESTCONTAINERS_RYUK_DISABLED=true)
	go test -tags integration -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-multiarch: ## Needs docker buildx; add --push to publish
	docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) -t $(IMAGE) .

clean:
	rm -rf bin
