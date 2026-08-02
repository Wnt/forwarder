module github.com/Wnt/forwarder

// Kept at the lowest Go the box might ship (Debian 13 ships 1.24, the sandbox
// 1.24.7) so a plain `go build` on the box never tries to download a newer
// toolchain. Deliberately no `toolchain` directive for the same reason — with
// vendored deps + GOTOOLCHAIN=local the build is hermetic and offline.
go 1.23

require (
	github.com/coder/websocket v1.8.14
	github.com/hashicorp/yamux v0.1.2
)
