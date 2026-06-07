// Command forwarder-server is the box-side half of the self-hosted reverse
// tunnel. It runs on the always-on lab box (vm-control), loopback-only, behind
// the lab's existing Caddy on :443:
//
//   - Caddy reverse-proxies the agent's control WebSocket and all public guest
//     traffic to this server's HTTP port (FORWARDER_HTTP_PORT, default 7080).
//   - Caddy's on-demand TLS asks the management port (FORWARDER_MGMT_PORT,
//     default 7001) /ask before minting a cert, so only live tunnel hostnames
//     (+ the control host) ever get one.
//
// Because Caddy already owns :443 and a valid Let's Encrypt cert, this server
// terminates no TLS of its own and opens no public port — nothing new in the box
// firewall. It is application-agnostic: any guest project runs the agent and
// registers its own *.lab.madekivi.fi subdomain.
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Wnt/stream-connect/lab/forwarder/internal/server"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("forwarder-server: ")

	bindHost := env("FORWARDER_BIND_HOST", "127.0.0.1")
	httpPort := envInt("FORWARDER_HTTP_PORT", 7080)
	mgmtPort := envInt("FORWARDER_MGMT_PORT", 7001)
	controlHost := env("FORWARDER_CONTROL_HOST", "")

	if os.Getenv("FORWARDER_AGENT_TOKEN") == "" {
		log.Fatal("FORWARDER_AGENT_TOKEN is required")
	}
	if controlHost == "" {
		log.Print("WARN: FORWARDER_CONTROL_HOST is empty — Caddy cannot obtain a " +
			"cert for the agent's control hostname; set it to the FQDN the agent " +
			"dials (e.g. tunnel.lab.madekivi.fi)")
	}

	s := server.New(server.Config{
		ControlPath:       env("FORWARDER_CONTROL_PATH", "/__forwarder/v1/control"),
		ControlHost:       controlHost,
		AgentToken:        os.Getenv("FORWARDER_AGENT_TOKEN"),
		MaxConnsPerTunnel: envInt("FORWARDER_MAX_CONNS_PER_TUNNEL", 256),
	}, log.Printf)

	// Management API — loopback only. Caddy's ask hits /ask; ops read /status.
	mgmtAddr := bindHost + ":" + strconv.Itoa(mgmtPort)
	go func() {
		log.Printf("management API on http://%s (ask/status/healthz)", mgmtAddr)
		srv := &http.Server{Addr: mgmtAddr, Handler: s.ManagementHandler(), ReadHeaderTimeout: 10 * time.Second}
		log.Fatalf("management listener: %v", srv.ListenAndServe())
	}()

	// Public + control listener. No WriteTimeout: the control WS and proxied
	// guest WebSockets are long-lived. ReadHeaderTimeout still guards slowloris.
	httpAddr := bindHost + ":" + strconv.Itoa(httpPort)
	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	log.Printf("ingress on http://%s", httpAddr)
	log.Fatalf("ingress listener: %v", srv.ListenAndServe())
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
