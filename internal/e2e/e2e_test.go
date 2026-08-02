// Package e2e drives the real server and agent against each other over a real
// (loopback) WebSocket + yamux, proving the whole Phase-1 path: control
// handshake, registration, plain-HTTP proxying, and unbuffered WebSocket
// passthrough of a large binary frame (the JPEG-stream shape).
package e2e

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wnt/forwarder/internal/agent"
	"github.com/Wnt/forwarder/internal/framing"
	"github.com/Wnt/forwarder/internal/server"
	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

const (
	token   = "test-secret"
	pubHost = "ios.test"
)

// discard is the logger handed to the server/agent: their goroutines outlive the
// test body (teardown is async), so logging via t.Logf from them would race the
// test's completion. Assertions don't need the logs; t.Fatalf carries failures.
func discard(string, ...any) {}

// backend stands in for the guest app on localhost: a /hello HTTP route and a
// /ws echo route that imposes no payload limit.
func newBackend(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		fmt.Fprintf(w, "hello via %s xff=%s", r.Header.Get("X-Forwarded-Proto"), r.Header.Get("X-Forwarded-For"))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		c.SetReadLimit(-1) // endpoint, not the forwarder, frames WS
		defer c.Close(websocket.StatusNormalClosure, "")
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// clientVia returns an *http.Client whose dials all land on addr regardless of
// the request URL host — so we can present Host: ios.test to a loopback server,
// exactly as Caddy presents the original Host to the forwarder.
func clientVia(addr string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}
}

func TestEndToEnd(t *testing.T) {
	backend := newBackend(t)
	backendPort := mustPort(t, backend.URL)

	// Forwarder: ingress handler (what Caddy proxies to) + management handler.
	s := server.New(server.Config{AgentToken: token, ControlHost: "tunnel.test"}, discard)
	ingress := httptest.NewServer(s.Handler())
	t.Cleanup(ingress.Close)
	mgmt := httptest.NewServer(s.ManagementHandler())
	t.Cleanup(mgmt.Close)

	ingressAddr := mustHost(t, ingress.URL)

	// Agent dials the ingress control path and registers the public host.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agent.Run(ctx, agent.Config{
		ServerURL: agent.ControlURL("ws://" + ingressAddr + "/__forwarder/v1/control"),
		Token:     token,
		Tunnels:   []framing.TunnelDef{{ID: "web", Proto: framing.ProtoHTTP, Hostname: pubHost, LocalPort: backendPort}},
		Logf:      discard,
	})

	waitRegistered(t, mgmt.URL, pubHost)

	t.Run("ask guard", func(t *testing.T) {
		if code := askCode(t, mgmt.URL, pubHost); code != 200 {
			t.Fatalf("registered host should be allowed, got %d", code)
		}
		if code := askCode(t, mgmt.URL, "tunnel.test"); code != 200 {
			t.Fatalf("control host should be allowed, got %d", code)
		}
		if code := askCode(t, mgmt.URL, "evil.test"); code == 200 {
			t.Fatalf("unregistered host must be refused, got %d", code)
		}
	})

	t.Run("http proxy", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://"+pubHost+"/hello", nil)
		resp, err := clientVia(ingressAddr).Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Seen-Host"); got != pubHost {
			t.Fatalf("backend saw Host %q, want %q (Host must be preserved)", got, pubHost)
		}
		if !strings.HasPrefix(string(body), "hello via https") {
			t.Fatalf("body %q missing X-Forwarded-Proto=https", body)
		}
	})

	t.Run("http unknown host -> 502", func(t *testing.T) {
		req, _ := http.NewRequest("GET", "http://nope.test/hello", nil)
		resp, err := clientVia(ingressAddr).Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("unregistered host: status %d, want 502", resp.StatusCode)
		}
	})

	t.Run("websocket large binary frame", func(t *testing.T) {
		c, _, err := websocket.Dial(ctx, "ws://"+pubHost+"/ws", &websocket.DialOptions{HTTPClient: clientVia(ingressAddr)})
		if err != nil {
			t.Fatalf("ws dial: %v", err)
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		c.SetReadLimit(-1)

		// 256 KB > yamux's io.Copy chunk and > the default 32 KB WS read cap: if
		// the forwarder buffered or re-framed, this would clip or stall.
		payload := make([]byte, 256*1024)
		rand.Read(payload)
		if err := c.Write(ctx, websocket.MessageBinary, payload); err != nil {
			t.Fatalf("ws write: %v", err)
		}
		typ, got, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if typ != websocket.MessageBinary || len(got) != len(payload) {
			t.Fatalf("echo type=%v len=%d, want binary len=%d", typ, len(got), len(payload))
		}
		for i := range payload {
			if got[i] != payload[i] {
				t.Fatalf("echo differs at byte %d", i)
			}
		}
	})
}

// TestEndToEndTCP exercises the raw TCP passthrough path: the server opens a
// public TCP listener for a registered tcp tunnel, and a raw client connection to
// it round-trips through the agent to a local TCP echo server.
func TestEndToEndTCP(t *testing.T) {
	// Local TCP echo backend (stands in for, e.g., sshd on the guest).
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	echoPort := echo.Addr().(*net.TCPAddr).Port

	// The public TCP listen port — bound on loopback for the test. Pick a free one
	// and pin the policy range to exactly it.
	pubPort := freeTCPPort(t)
	s := server.New(server.Config{
		AgentToken: token, ControlHost: "tunnel.test",
		TCPMinPort: pubPort, TCPMaxPort: pubPort, TCPBind: "127.0.0.1",
	}, discard)
	ingress := httptest.NewServer(s.Handler())
	t.Cleanup(ingress.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agent.Run(ctx, agent.Config{
		ServerURL: agent.ControlURL("ws://" + mustHost(t, ingress.URL) + "/__forwarder/v1/control"),
		Token:     token,
		Tunnels:   []framing.TunnelDef{{ID: "echo", Proto: framing.ProtoTCP, RemotePort: pubPort, LocalPort: echoPort}},
		Logf:      discard,
	})

	// Wait for the public listener to come up (it's opened on registration), then
	// round-trip raw bytes through it.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(pubPort))
	var conn net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			conn = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("public tcp port never opened")
	}
	defer conn.Close()

	msg := []byte("hello over a raw tcp tunnel")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestAuthRejected(t *testing.T) {
	s := server.New(server.Config{AgentToken: token, ControlHost: "tunnel.test"}, discard)
	ingress := httptest.NewServer(s.Handler())
	t.Cleanup(ingress.Close)

	ctx := context.Background()
	c, _, err := websocket.Dial(ctx,
		"ws://"+mustHost(t, ingress.URL)+"/__forwarder/v1/control",
		&websocket.DialOptions{Subprotocols: []string{framing.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)

	// Speak the real control protocol (yamux + a control stream) with a bad
	// token; expect a rejecting hello-ack.
	sess, err := yamux.Client(nc, framing.YamuxConfig(io.Discard))
	if err != nil {
		t.Fatalf("yamux: %v", err)
	}
	defer sess.Close()
	st, err := sess.OpenStream()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := framing.WriteJSONLine(st, framing.HelloMsg{Type: framing.TypeHello, Token: "wrong", Version: "1"}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	var ack framing.HelloAckMsg
	if err := framing.ReadJSONLine(bufio.NewReader(st), &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Error != "unauthorized" {
		t.Fatalf("expected unauthorized ack, got %+v", ack)
	}
}

// --- helpers ---------------------------------------------------------------

func waitRegistered(t *testing.T, mgmtURL, host string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if askCode(t, mgmtURL, host) == 200 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("host %q never registered", host)
}

func askCode(t *testing.T, mgmtURL, domain string) int {
	t.Helper()
	resp, err := http.Get(mgmtURL + "/ask?domain=" + url.QueryEscape(domain))
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func mustPort(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(u.Host)
	var n int
	fmt.Sscanf(p, "%d", &n)
	return n
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
