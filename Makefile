# LocalMesh — build, test, release

BINARY_SERVER := localmesh-server
BINARY_AGENT  := localmesh-agent
BIN           := bin
VERSION       ?= dev
LDFLAGS       := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet tidy clean release dashboard dev-up dev-down

all: build

build:
	@mkdir -p $(BIN)
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY_SERVER) ./cmd/localmesh-server
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY_AGENT)  ./cmd/localmesh-agent

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BIN) dist

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
