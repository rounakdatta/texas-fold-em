.PHONY: all build test vet fmt cover clean

all: build

build:
	go build -ldflags="-s -w" -o texas-fold-em .

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
