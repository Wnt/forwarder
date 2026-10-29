package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wnt/forwarder/internal/agent"
	"github.com/Wnt/forwarder/internal/framing"
	"github.com/Wnt/forwarder/internal/server"
	"github.com/coder/websocket"
)

func policyTunnel(t *testing.T, backend *httptest.Server, max int) (*httptest.Server, *http.Client) {
	t.Helper()
	headerTimeout := 1
	if max > 8 {
		headerTimeout = 10
	}
	s := server.New(server.Config{AgentToken: token, HostPolicies: map[string]server.HostPolicy{
		pubHost: {MaxConns: max, HeaderTimeoutSeconds: headerTimeout, AllowedOrigins: []string{"https://wnt.github.io"}},
	}}, discard)
	ingress := httptest.NewServer(s.Handler())
	t.Cleanup(ingress.Close)
	mgmt := httptest.NewServer(s.ManagementHandler())
	t.Cleanup(mgmt.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go agent.Run(ctx, agent.Config{
		ServerURL: "ws://" + mustHost(t, ingress.URL) + "/__forwarder/v1/control", Token: token,
		Tunnels: []framing.TunnelDef{{ID: "web", Proto: framing.ProtoHTTP, Hostname: pubHost, LocalPort: mustPort(t, backend.URL)}},
	})
	waitRegistered(t, mgmt.URL, pubHost)
	client := clientVia(mustHost(t, ingress.URL))
	client.Timeout = 15 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	return ingress, client
}

func TestHostGatewayErrors(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone":
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
		case "/timeout":
			<-r.Context().Done()
		}
	}))
	t.Cleanup(backend.Close)
	_, client := policyTunnel(t, backend, 8)
	for _, path := range []string{"/gone", "/timeout"} {
		for _, upgrade := range []bool{false, true} {
			r, _ := http.NewRequest("GET", "http://"+pubHost+path, nil)
			r.Header.Set("Origin", "https://wnt.github.io")
			if upgrade {
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", "websocket")
			}
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			want := 502
			if path == "/timeout" {
				want = 504
			}
			if resp.StatusCode != want || resp.Header.Get("Access-Control-Allow-Origin") != "https://wnt.github.io" || resp.Header.Get("Retry-After") != "5" {
				t.Fatalf("%s upgrade=%v code=%d headers=%v", path, upgrade, resp.StatusCode, resp.Header)
			}
		}
	}
}

func TestHostSlowUploadTimeout(t *testing.T) {
	backend := newBackend(t)
	ingress, _ := policyTunnel(t, backend, 8)
	c, err := net.Dial("tcp", mustHost(t, ingress.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "POST /hello HTTP/1.1\r\nHost: %s\r\nOrigin: https://wnt.github.io\r\nContent-Length: 1\r\n\r\n", pubHost)
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 504 || r.Header.Get("Access-Control-Allow-Origin") != "https://wnt.github.io" || r.Header.Get("Retry-After") != "5" {
		t.Fatalf("slow upload: code=%d headers=%v", r.StatusCode, r.Header)
	}
}

func TestHostWebSocketOutlivesHeaderTimeout(t *testing.T) {
	_, client := policyTunnel(t, newBackend(t), 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+pubHost+"/ws", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	time.Sleep(1200 * time.Millisecond)
	if err := c.Write(ctx, websocket.MessageText, []byte("still live")); err != nil {
		t.Fatal(err)
	}
	_, body, err := c.Read(ctx)
	if err != nil || string(body) != "still live" {
		t.Fatalf("socket did not outlive header deadline: %q %v", body, err)
	}
}

// 300 stalled streams exceed the old cap. Their per-stream flow control must
// leave a small fetch and a 5 MB WebSocket echo free to complete on the session.
func TestCapacityAndStreamIsolation(t *testing.T) {
	arrived := make(chan struct{}, 300)
	resume := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			w.Write([]byte(strings.Repeat("s", 8<<20)))
			return
		}
		if r.URL.Path == "/hold" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			arrived <- struct{}{}
			select {
			case <-resume:
			case <-r.Context().Done():
			}
			return
		}
		if r.URL.Path == "/ws" {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer c.CloseNow()
			c.SetReadLimit(-1)
			typ, body, err := c.Read(r.Context())
			if err == nil {
				c.Write(r.Context(), typ, body)
			}
			return
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	defer close(resume)
	_, client := policyTunnel(t, backend, 320)
	// Leave response bodies unread so these streams exhaust their receive
	// windows. Other streams must continue even while those writers are blocked.
	for i := 0; i < 10; i++ {
		resp, err := client.Get("http://" + pubHost + "/slow")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
	}
	var wg sync.WaitGroup
	errs := make(chan error, 300)
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := client.Get("http://" + pubHost + "/hold")
			if err == nil {
				_, err = io.Copy(io.Discard, r.Body)
				r.Body.Close()
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	for i := 0; i < 300; i++ {
		select {
		case <-arrived:
		case err := <-errs:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("300 requests did not reach the app")
		}
	}
	r, err := client.Get("http://" + pubHost + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("independent HTTP stream: %q %v", body, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+pubHost+"/ws", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(-1)
	payload := []byte(strings.Repeat("x", 5<<20))
	if err := c.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	_, got, err := c.Read(ctx)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("large WS stream: bytes=%d err=%v", len(got), err)
	}
	// Release and join the held requests before test teardown.
	for i := 0; i < 300; i++ {
		resume <- struct{}{}
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatal(<-errs)
	}
}
