.PHONY: all build test vet fmt lint clean interop interop-stress interop-fault interop-chain

# Build tag conventions for tests/interop/:
#   interop           — default interop suite, runs against default compose
#   interop_multicast — Scout / multicast-enabled scenarios
#   stress            — scale / large payload / high-rate; opt-in, nightly CI
#   fault             — toxiproxy fault injection; opt-in
#   chain             — two-router topology; opt-in

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

# Multicast-enabled zenohd (host networking, Linux only) for Scout
# tests and peer-multicast pub/sub interop. The compose file also
# brings up a host-networked Python sidecar that the multicast pub/sub
# interop test drives via `docker compose exec`.
interop-multicast-up:
	docker compose -f tests/docker-compose.multicast.yml up -d --build --wait

interop-multicast-down:
	docker compose -f tests/docker-compose.multicast.yml down

interop-multicast-test: interop-multicast-up
	go test -race -tags interop_multicast -count=1 -v ./tests/interop/...

# Stress suite: large payloads, 100+ entities, high-rate publish. Runs on
# the default compose; ~minutes scale, not for every-commit CI.
interop-stress: interop-up
	go test -race -tags "interop stress" -count=1 -timeout 30m -v ./tests/interop/... ./tests/stress/...

# Fault-injection suite: toxiproxy-driven latency / bandwidth / half-open
# / unreachable scenarios. Uses its own compose (zenohd + toxiproxy).
interop-fault-up:
	docker compose -f tests/docker-compose.fault.yml up -d --build --wait

interop-fault-down:
	docker compose -f tests/docker-compose.fault.yml down

interop-fault-test: interop-fault-up
	go test -race -tags "interop fault" -count=1 -timeout 10m -run 'Fault' -v ./tests/interop/...

# Two-router chain suite: pub/sub/get and alias resolution across
# zenohd1 → zenohd2.
interop-chain-up:
	docker compose -f tests/docker-compose.chain.yml up -d --build --wait

interop-chain-down:
	docker compose -f tests/docker-compose.chain.yml down

interop-chain-test: interop-chain-up
	go test -race -tags "interop chain" -count=1 -timeout 5m -run 'Chain' -v ./tests/interop/...
