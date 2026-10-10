# LocalMesh — build, test, release

BINARY_SERVER := localmesh-server
BINARY_AGENT  := localmesh-agent
BIN           := bin
VERSION       ?= dev
LDFLAGS       := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet tidy clean clean-test-resources release dashboard dev-up dev-down verify verify-fast

all: build

build:
	@mkdir -p $(BIN)
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY_SERVER) ./cmd/localmesh-server
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY_AGENT)  ./cmd/localmesh-agent

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
	go test ./...
	go test -race -count=1 ./internal/server/ ./internal/server/scheduler/
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
	go test -count=1 ./internal/server/
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

# Cross-compile node agents for the supported platforms.
release:
	@mkdir -p dist
	GOOS=darwin  GOARCH=arm64  go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY_AGENT)-darwin-arm64  ./cmd/localmesh-agent
	GOOS=darwin  GOARCH=amd64  go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY_AGENT)-darwin-amd64  ./cmd/localmesh-agent
	GOOS=linux   GOARCH=amd64  go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY_AGENT)-linux-amd64   ./cmd/localmesh-agent
	GOOS=linux   GOARCH=arm64  go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY_AGENT)-linux-arm64   ./cmd/localmesh-agent
	GOOS=windows GOARCH=amd64  go build -ldflags '$(LDFLAGS)' -o dist/$(BINARY_AGENT)-windows-amd64.exe ./cmd/localmesh-agent
	cd dist && shasum -a 256 * > SHA256SUMS

dashboard:
	cd web && npm install && npm run build

# Local development stack: postgres + control plane on :8080 (insecure HTTP).
dev-up:
	docker compose -f deploy/dev.compose.yml up -d
	@echo "control plane: http://localhost:8080   dashboard: cd web && npm run dev"

dev-down:
	docker compose -f deploy/dev.compose.yml down
