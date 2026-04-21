.PHONY: all build test vet fmt lint clean interop

all: build test

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	go vet ./...
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed, skipping"

clean:
	go clean ./...

# Bring up zenohd + python for interop tests
interop-up:
	docker-compose -f tests/docker-compose.yml up -d

interop-down:
	docker-compose -f tests/docker-compose.yml down

interop-test: interop-up
	go test -race -tags interop ./tests/interop/...
