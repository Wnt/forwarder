# Self-hosted forwarder build.
#
# Vendored deps + GOTOOLCHAIN=local keep every build hermetic and offline — the
# 1 GB lab box builds the server during redeploy with no module fetch and no
# toolchain download. `make agents` cross-compiles the agent for the machines a
# guest project runs on (Apple Silicon / Intel Macs, Linux).
GO       ?= go
GOFLAGS  ?= -mod=vendor
LDFLAGS  ?= -s -w
BIN      ?= bin
export GOTOOLCHAIN = local

.PHONY: all server agents test vet tidy clean

all: server agents

# The box-side binary (Linux). redeploy.sh runs exactly this build on vm-control.
server:
	mkdir -p $(BIN)
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/forwarder-server ./cmd/forwarder-server

# Guest-side binaries for the machine the app runs on.
agents:
	mkdir -p $(BIN)
	GOOS=darwin GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/forwarder-agent-darwin-arm64 ./cmd/forwarder-agent
	GOOS=darwin GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/forwarder-agent-darwin-amd64 ./cmd/forwarder-agent
	GOOS=linux  GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/forwarder-agent-linux-amd64  ./cmd/forwarder-agent

test:
	$(GO) test $(GOFLAGS) ./...

vet:
	$(GO) vet $(GOFLAGS) ./...

tidy:
	GOFLAGS= $(GO) mod tidy
	GOFLAGS= $(GO) mod vendor

clean:
	rm -rf $(BIN)
