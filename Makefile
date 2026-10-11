# LocalMesh — build, test, release

BINARY_SERVER := localmesh-server
BINARY_AGENT  := localmesh-agent
BINARY_ROUTER := localmesh-router
BIN           := bin
VERSION       ?= dev
LDFLAGS       := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet tidy clean clean-test-resources bench release dashboard dev-up dev-down verify verify-fast verify-stress chaos-test

all: build

build:
	@mkdir -p $(BIN)
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY_SERVER) ./cmd/localmesh-server
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY_AGENT)  ./cmd/localmesh-agent
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY_ROUTER) ./cmd/localmesh-router

test:
	go test ./...

# Regression gate — run before merging any workstream and before calling
# a milestone done. Covers formatting, static analysis, the full backend
# suite (incl. security regression tests), race detection on the server,
# agent cross-compiles, and every frontend check. Fails fast on the
# cheapest checks first.
verify:
	@test -z "$$(gofmt -l internal cmd 2>/dev/null)" || (echo "gofmt dirty:"; gofmt -l internal cmd; exit 1)
	go vet ./...
	go test -timeout 15m ./...
	go test -race -timeout 15m -count=1 ./internal/server/ ./internal/server/scheduler/
	@for t in "darwin arm64" "darwin amd64" "linux amd64" "linux arm64" "windows amd64"; do \
		set -- $$t; GOOS=$$1 GOARCH=$$2 go build -o /dev/null ./cmd/localmesh-agent || exit 1; \
	done
	@echo "cross-compile ok (5 targets)"
	cd web && npx tsc --noEmit && npm run lint && npm run build
	@echo "verify: PASS"

# Backend-only gate — the loop to run after every integration step.
verify-fast:
	@test -z "$$(gofmt -l internal cmd 2>/dev/null)" || (echo "gofmt dirty:"; gofmt -l internal cmd; exit 1)
	go vet ./...
	go test -timeout 15m -count=1 ./internal/server/
	@echo "verify-fast: PASS"

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN) dist

# Sweep ONLY test-owned Docker resources (label localmesh.test=1) —
# containers are tmpfs-backed so no anonymous volumes are created.
# Reports dangling leftovers for manual review; never blanket-prunes.
clean-test-resources:
	@bash scripts/clean-test-resources.sh

# Infrastructure benchmarks — scheduler scale, op-claim latency,
# gateway overhead vs engine, conn churn. Real postgres containers;
# ~2-3 min. Override: BENCHTIME=500ms COUNT=2 make bench
bench:
	@bash scripts/bench.sh

# Distributed-failure suite: build-tagged chaos tests (connection drops,
# fencing violations, postgres restart, promotion races, interrupted
# updates). Heavy — real postgres containers, kills mid-flight. Isolated
# from the default `go test ./...` run.
chaos-test:
	@bash scripts/ci-chaos.sh

# Repeated runs of the historically flaky integration/chaos tests —
# the stability gate. -n ROUNDS (default 10), -l N cpu-burner load.
verify-stress:
	@bash scripts/ci-stress.sh

# Versioned release: all 5 agent targets + server binaries, SHA256SUMS,
# manifest, signature. Requires LOCALMESH_SIGNING_KEY (minisign secret
# key path) — without it the script refuses; dev-only unsigned output
# needs an explicit `scripts/release.sh --unsigned`. See docs/releases.md.
# VERSION=v1.2.3 or a git tag/describe fallback.
release:
	@bash scripts/release.sh

dashboard:
	cd web && npm install && npm run build

# Local development stack: postgres + control plane on :8080 (insecure HTTP).
dev-up:
	docker compose -f deploy/dev.compose.yml up -d
	@echo "control plane: http://localhost:8080   dashboard: cd web && npm run dev"

dev-down:
	docker compose -f deploy/dev.compose.yml down
