GO ?= go
BIN := bin

.PHONY: all build agent test e2e vet fmt clean

all: vet test build

# sbxd is built on its own: `go build ./cmd/...` would also rebuild
# sbx-agent with cgo, replacing the static binary with a dynamically
# linked one that cannot start in a musl image.
build: agent
	$(GO) build -trimpath -o $(BIN)/sbxd ./cmd/sbxd
	$(GO) build -trimpath -o $(BIN)/sbx-mcp ./cmd/sbx-mcp

# The in-sandbox agent must be static: it is bind-mounted into arbitrary images.
agent:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o $(BIN)/sbx-agent ./cmd/sbx-agent

test:
	$(GO) test ./...

# The end-to-end test runs the real server against rootless Podman and
# Postgres. It needs SBX_E2E_DATABASE_URL; see test/e2e/e2e_test.go.
e2e:
	SBX_E2E=1 $(GO) test -count=1 -v -timeout 15m ./test/e2e/

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf $(BIN)
