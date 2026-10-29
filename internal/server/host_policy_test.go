package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wnt/forwarder/internal/framing"
)

func TestHostErrorPolicy(t *testing.T) {
	s := New(Config{HostPolicies: map[string]HostPolicy{
		"app.test": {MaxConns: 2, AllowedOrigins: []string{"https://wnt.github.io"}},
	}}, nil)
	for _, tc := range []struct {
		host, origin string
		cors, retry  bool
	}{
		{"app.test", "https://wnt.github.io", true, true},
		{"APP.TEST.:443", "https://wnt.github.io", true, true},
		{"app.test", "https://evil.test", false, true},
		{"app.test", "", false, true},
		{"app.test.evil", "https://wnt.github.io", false, false},
		{"other.test", "https://wnt.github.io", false, false},
	} {
		t.Run(tc.host+tc.origin, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+tc.host+"/coarse/health", nil)
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 502 {
				t.Fatalf("code = %d", w.Code)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); (got != "") != tc.cors || (tc.cors && got != tc.origin) {
				t.Fatalf("CORS = %q", got)
			}
			if (w.Header().Get("Retry-After") == "5") != tc.retry {
				t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
			}
			if tc.retry && (w.Header().Get("Vary") != "Origin" || w.Header().Get("Cache-Control") != "no-store") {
				t.Fatal("configured error must vary by Origin and forbid caching")
			}
		})
	}
}

func TestHostCapacityDoesNotChangeOtherGuests(t *testing.T) {
	s := New(Config{HostPolicies: map[string]HostPolicy{
		"app.test": {MaxConns: 3, AllowedOrigins: []string{"https://wnt.github.io"}},
	}}, nil)
	a := &agentSession{id: "a"}
	s.reg.register(a, []framing.TunnelDef{
		{ID: "app", Proto: framing.ProtoHTTP, Hostname: "app.test", LocalPort: 1},
		{ID: "other", Proto: framing.ProtoHTTP, Hostname: "other.test", LocalPort: 1},
	}, 1, tcpPolicy{})
	legacyRelease, ok := a.acquire()
	if !ok {
		t.Fatal("legacy slot unavailable")
	}
	defer legacyRelease()
	h := s.hosts["app.test"]
	for i := 0; i < 3; i++ {
		release, ok := h.acquire()
		if !ok {
			t.Fatal("scoped capacity shared the legacy limit")
		}
		defer release()
	}
	for _, host := range []string{"app.test", "other.test"} {
		r := httptest.NewRequest("OPTIONS", "http://"+host+"/coarse/health", nil)
		r.Header.Set("Origin", "https://wnt.github.io")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 503 || (w.Header().Get("Access-Control-Allow-Origin") != "") != (host == "app.test") {
			t.Fatalf("host %s code=%d headers=%v", host, w.Code, w.Header())
		}
	}
	w := httptest.NewRecorder()
	s.ManagementHandler().ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
	var view statusView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	v := view.HostLimits["app.test"]
	if v.Active != 3 || v.Limit != 3 || v.Peak != 3 || v.Rejected != 1 || v.Errors != 1 {
		t.Fatalf("metrics = %+v", v)
	}
}
