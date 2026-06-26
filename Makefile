.PHONY: all build test vet fmt cover clean sandbox

all: build

build:
	go build -ldflags="-s -w" -o texas-fold-em .

# Run locally against a snapshot of the live homelab staging.db for fast
# classifier iteration (no firefly/fold/MySQL needed). Set KUBECONFIG
# first (access-homelab-setup-k3s skill). REFRESH=1 re-pulls the snapshot.
sandbox:
	./scripts/local-sandbox.sh

test:
	go test ./... -race -count=1

vet:
	go vet ./...

fmt:
	gofmt -s -w .

cover:
	go test ./... -race -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -20

clean:
	rm -f texas-fold-em coverage.out
