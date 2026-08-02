module github.com/Wnt/forwarder

// Latest stable Go. deploy/install.sh installs this exact toolchain from
// go.dev on the box (Debian's golang-go lags — trixie ships 1.24), so the box,
// CI and a laptop all build with the same compiler. Deliberately no `toolchain`
// directive: with vendored deps + GOTOOLCHAIN=local the build stays hermetic and
// offline, and a version skew fails loudly instead of silently downloading.
go 1.26.5

require (
	github.com/coder/websocket v1.8.15
	github.com/hashicorp/yamux v0.1.2
)
