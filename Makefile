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
	@test -z "$$(gofmt -l .)" || ( echo "Code is not formatted. Run 'make fmt'"; exit 1 )
	go tool staticcheck ./...

clean:
	go clean ./...

# Bring up zenohd + python for interop tests
interop-up:
	docker compose -f tests/docker-compose.yml up -d --build --wait

interop-down:
	docker compose -f tests/docker-compose.yml down

interop-test: interop-up
	go test -race -tags interop -count=1 -v ./tests/interop/...

# Multicast-enabled zenohd (host networking, Linux only) for Scout tests.
interop-multicast-up:
	docker compose -f tests/docker-compose.multicast.yml up -d --wait

interop-multicast-down:
	docker compose -f tests/docker-compose.multicast.yml down

interop-multicast-test: interop-multicast-up
	go test -race -tags interop_multicast -count=1 -v ./tests/interop/...
