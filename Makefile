GO ?= go
BIN := bin

.PHONY: all build agent test vet fmt clean

all: vet test build

build: agent
	$(GO) build -o $(BIN)/ ./cmd/...

# The in-sandbox agent must be static: it is bind-mounted into arbitrary images.
agent:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o $(BIN)/sbx-agent ./cmd/sbx-agent

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf $(BIN)
