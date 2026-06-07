package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wnt/stream-connect/lab/forwarder/internal/framing"
)

func TestCanonHost(t *testing.T) {
	cases := map[string]string{
		"ios.lab.madekivi.fi":     "ios.lab.madekivi.fi",
		"IOS.Lab.Madekivi.FI":     "ios.lab.madekivi.fi",
		"ios.lab.madekivi.fi:443": "ios.lab.madekivi.fi",
		"ios.lab.madekivi.fi.":    "ios.lab.madekivi.fi",
		"  ios.lab.madekivi.fi  ": "ios.lab.madekivi.fi",
	}
	for in, want := range cases {
		if got := canonHost(in); got != want {
			t.Errorf("canonHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegisterConflictAndRemove(t *testing.T) {
	r := newRegistry()
	a1 := &agentSession{id: "a1"}
	got := r.register(a1, []framing.TunnelDef{
		{ID: "web", Proto: framing.ProtoHTTP, Hostname: "ios.test", LocalPort: 8080},
		{ID: "bad", Proto: framing.ProtoHTTP, Hostname: "", LocalPort: 8080},
		{ID: "tcp", Proto: framing.ProtoTCP, RemotePort: 2222, LocalPort: 22},
	}, 8)
	if got[0].Error != "" || got[0].RemoteAddr != "ios.test:443" {
		t.Fatalf("web tunnel: %+v", got[0])
	}
	if got[1].Error == "" {
		t.Fatalf("missing hostname should be rejected")
	}
	if got[2].Error == "" {
		t.Fatalf("tcp should be rejected in Phase 1")
	}
	if !r.hostRegistered("ios.test") {
		t.Fatalf("ios.test should be registered")
	}

	// A second agent cannot steal the hostname.
	a2 := &agentSession{id: "a2"}
	got2 := r.register(a2, []framing.TunnelDef{{ID: "web", Proto: framing.ProtoHTTP, Hostname: "ios.test", LocalPort: 9090}}, 8)
	if got2[0].Error == "" {
		t.Fatalf("duplicate hostname should be rejected")
	}

	// Removing a1 frees the host; removing an unknown id is a no-op.
	r.removeAgent("a1")
	if r.hostRegistered("ios.test") {
		t.Fatalf("ios.test should be free after removeAgent")
	}
	r.removeAgent("nope")
}

func TestAcquireLimit(t *testing.T) {
	a := &agentSession{sem: make(chan struct{}, 2)}
	r1, ok1 := a.acquire()
	_, ok2 := a.acquire()
	_, ok3 := a.acquire()
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("cap 2: got %v %v %v", ok1, ok2, ok3)
	}
	r1()
	if _, ok := a.acquire(); !ok {
		t.Fatalf("release should free a slot")
	}
}

func TestAskHandler(t *testing.T) {
	s := New(Config{AgentToken: "x", ControlHost: "tunnel.test"}, nil)
	a := &agentSession{id: "a"}
	s.reg.register(a, []framing.TunnelDef{{ID: "web", Proto: framing.ProtoHTTP, Hostname: "ios.test", LocalPort: 1}}, 8)

	for _, tc := range []struct {
		domain string
		want   int
	}{
		{"ios.test", 200},
		{"tunnel.test", 200},
		{"evil.test", http.StatusForbidden},
		{"", http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		s.handleAsk(w, httptest.NewRequest("GET", "/ask?domain="+tc.domain, nil))
		if w.Code != tc.want {
			t.Errorf("ask %q = %d, want %d", tc.domain, w.Code, tc.want)
		}
	}
}

func TestAuthLimiter(t *testing.T) {
	l := newAuthLimiter()
	ip := "1.2.3.4"
	for i := 0; i < 5; i++ {
		if l.banned(ip) {
			t.Fatalf("banned too early at %d", i)
		}
		l.fail(ip)
	}
	if !l.banned(ip) {
		t.Fatalf("should be banned after 5 failures")
	}
	// A success clears state for a (different) IP path.
	l.ok("5.6.7.8")
}
