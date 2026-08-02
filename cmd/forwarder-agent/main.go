// Command forwarder-agent is the guest-side half of the self-hosted reverse
// tunnel. It runs next to the application being exposed (e.g. the iOS-pwa-runner
// on a Mac behind NAT), dials OUT to the lab box over wss (so it works through
// any NAT/firewall), authenticates, registers its tunnels, and then forwards
// each public connection the server pushes to a local port.
//
// It is application-agnostic: point it at any local HTTP/WS port and a public
// hostname that resolves to the lab box (a *.lab.madekivi.fi subdomain).
//
// Usage:
//
//	forwarder-agent \
//	  --server wss://tunnel.lab.madekivi.fi/__forwarder/v1/control \
//	  --token  "$FORWARDER_AGENT_TOKEN" \
//	  --tunnel web:http:ios.lab.madekivi.fi:8080
//
// Flags fall back to env vars (FORWARDER_SERVER, FORWARDER_AGENT_TOKEN,
// FORWARDER_TUNNELS) so it slots into a .env-driven launcher.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Wnt/forwarder/internal/agent"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("forwarder-agent: ")

	var (
		server   = flag.String("server", os.Getenv("FORWARDER_SERVER"), "control endpoint: a full ws(s):// URL, or host[:port] (defaults to wss://host/__forwarder/v1/control)")
		token    = flag.String("token", os.Getenv("FORWARDER_AGENT_TOKEN"), "shared bearer secret (FORWARDER_AGENT_TOKEN)")
		tunnels  = flag.String("tunnel", os.Getenv("FORWARDER_TUNNELS"), "comma-separated tunnels: id:http:hostname:localport")
		insecure = flag.Bool("insecure", false, "skip TLS verification of the server (dev only)")
	)
	flag.Parse()

	if *server == "" || *token == "" || *tunnels == "" {
		log.Fatal("need --server, --token and --tunnel (or FORWARDER_SERVER/FORWARDER_AGENT_TOKEN/FORWARDER_TUNNELS)")
	}
	defs, err := agent.ParseTunnels(*tunnels)
	if err != nil {
		log.Fatalf("bad --tunnel: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	agent.Run(ctx, agent.Config{
		ServerURL: agent.ControlURL(*server),
		Token:     *token,
		Tunnels:   defs,
		Insecure:  *insecure,
		Logf:      log.Printf,
	})
	log.Print("shutting down")
}
