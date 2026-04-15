# texas-fold-em

BINARY  := texas-fold-em
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

IMAGE    ?= ghcr.io/rounakdatta/texas-fold-em
IMAGETAG ?= $(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: all build install test vet fmt cover run clean tidy image image-push helm-lint helm-template helm-package

all: vet test build

# --- native build / test --------------------------------------------------

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

install:
	go install -ldflags="$(LDFLAGS)" .

test:
	go test ./... -race -count=1

cover:
	go test ./... -race -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -20

vet:
	go vet ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

run: build
	@test -n "$$TEXAS_FOLDEM_BROKER_KEY" || { echo "set TEXAS_FOLDEM_BROKER_KEY"; exit 1; }
	@test -n "$$TEXAS_FOLDEM_ADMIN_KEY"  || { echo "set TEXAS_FOLDEM_ADMIN_KEY"; exit 1; }
	./$(BINARY)

# --- container ------------------------------------------------------------

# Single-arch local build (fast, for dev).
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(IMAGETAG) -t $(IMAGE):latest .

# Multi-arch build+push (for cluster use). Requires buildx + a logged-in
# registry: `docker buildx create --use` once, then `docker login ghcr.io`.
image-push:
	docker buildx build \
	    --build-arg VERSION=$(VERSION) \
	    --platform=$(PLATFORMS) \
	    -t $(IMAGE):$(IMAGETAG) \
	    -t $(IMAGE):latest \
	    --push \
	    .

# --- helm chart -----------------------------------------------------------

CHART_DIR := charts/texas-fold-em

helm-lint:
	helm lint $(CHART_DIR) --set secret.brokerKey=dummy --set secret.adminKey=dummy

helm-template:
	helm template dev $(CHART_DIR) --set secret.brokerKey=dummy --set secret.adminKey=dummy

helm-package:
	helm package $(CHART_DIR) -d dist/

clean:
	rm -rf $(BINARY) coverage.out dist/
