package e2e

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wnt/forwarder/internal/agent"
	"github.com/Wnt/forwarder/internal/framing"
	"github.com/Wnt/forwarder/internal/server"
)

// TestStreamingClientGoneClosesUpstream: a long-lived streaming response (SSE)
// must end at the app as soon as the public client goes away, not when the app
// next tries to write. The app here pings rarely (like an EventSource that pings
// every 30 s), so a proxy that only notices the client on a failed write would
// hold the stream — and the app's per-account slot — far past the test deadline.
//
// Both client shapes count as "gone": a full close, and a half close where the
// peer stops sending but leaves its read side open (what a proxy in front, or a
// TCP-mode load balancer, can leave behind). Either way the forwarder's request
// context is cancelled, and that alone must tear the tunnel stream down.
func TestStreamingClientGoneClosesUpstream(t *testing.T) {
	for _, mode := range []string{"close", "halfclose"} {
		t.Run(mode, func(t *testing.T) {
			appGone := make(chan struct{})
			mux := http.NewServeMux()
			mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
				defer close(appGone)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				fmt.Fprint(w, ": hello\n\n")
				w.(http.Flusher).Flush()
				tick := time.NewTicker(30 * time.Second)
				defer tick.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-tick.C:
						if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					}
				}
			})
			backend := httptest.NewServer(mux)
			t.Cleanup(backend.Close)

			s := server.New(server.Config{AgentToken: token, ControlHost: "tunnel.test"}, discard)
			ingress := httptest.NewServer(s.Handler())
			t.Cleanup(ingress.Close)
			mgmt := httptest.NewServer(s.ManagementHandler())
			t.Cleanup(mgmt.Close)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go agent.Run(ctx, agent.Config{
				ServerURL: agent.ControlURL("ws://" + mustHost(t, ingress.URL) + "/__forwarder/v1/control"),
				Token:     token,
				Tunnels:   []framing.TunnelDef{{ID: "web", Proto: framing.ProtoHTTP, Hostname: pubHost, LocalPort: mustPort(t, backend.URL)}},
				Logf:      discard,
			})
			waitRegistered(t, mgmt.URL, pubHost)

			c, err := net.Dial("tcp", mustHost(t, ingress.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			fmt.Fprintf(c, "GET /sse HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\n\r\n", pubHost)
			br := bufio.NewReader(c)
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("reading stream head: %v", err)
				}
				if strings.Contains(line, ": hello") {
					break
				}
			}

			if mode == "close" {
				c.Close()
			} else {
				c.(*net.TCPConn).CloseWrite()
			}

			select {
			case <-appGone:
			case <-time.After(3 * time.Second):
				t.Fatalf("app stream still open 3 s after the client went away (%s)", mode)
			}
		})
	}
}
