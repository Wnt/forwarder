// Package server implements the box-side forwarder: it accepts the agent's
// control WebSocket, multiplexes public connections to it over yamux, and backs
// Caddy's on-demand-TLS ask guard. It terminates no TLS of its own — the lab's
// existing Caddy on :443 does that and reverse-proxies here on loopback.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Config tunes a Server. Zero values get sane defaults via New.
type Config struct {
	ControlPath       string // reserved path the agent's control WS upgrades on
	ControlHost       string // always cert-approved so the agent can dial in
	AgentToken        string // shared bearer secret (required)
	MaxConnsPerTunnel int    // per-agent concurrent public connection cap

	// Raw TCP passthrough (Phase 2). OFF unless TCPMaxPort > 0. TCP tunnels need a
	// real public listen port (they can't ride Caddy's HTTP front door), so this
	// is opt-in: an operator sets a port range and opens it in the host firewall.
	// A tcp tunnel's remote_port must fall in [TCPMinPort, TCPMaxPort].
	TCPMinPort int
	TCPMaxPort int
	TCPBind    string // interface TCP tunnel listeners bind (default 0.0.0.0)
	PublicHost string // optional pretty host for the assigned TCP RemoteAddr label
}

// tcpPolicy is the registry's view of the TCP config.
func (c Config) tcpPolicy() tcpPolicy {
	return tcpPolicy{
		enabled: c.TCPMaxPort > 0,
		min:     c.TCPMinPort,
		max:     c.TCPMaxPort,
		bind:    c.TCPBind,
		label:   c.PublicHost,
	}
}

// Server is safe for concurrent use once constructed.
type Server struct {
	cfg     Config
	reg     *registry
	limiter *authLimiter
	log     func(string, ...any)
}

// New builds a Server. A nil logf discards logs.
func New(cfg Config, logf func(string, ...any)) *Server {
	if cfg.ControlPath == "" {
		cfg.ControlPath = "/__forwarder/v1/control"
	}
	if cfg.MaxConnsPerTunnel <= 0 {
		cfg.MaxConnsPerTunnel = 256
	}
	if cfg.TCPBind == "" {
		cfg.TCPBind = "0.0.0.0"
	}
	cfg.ControlHost = canonHost(cfg.ControlHost)
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{cfg: cfg, reg: newRegistry(), limiter: newAuthLimiter(), log: logf}
}

// Handler is the public+control ingress handler Caddy reverse-proxies to.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.handle) }

// ManagementHandler is the loopback ops/ask API: /ask, /status, /healthz.
func (s *Server) ManagementHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ask", s.handleAsk)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.reg.snapshot())
	})
	return mux
}

func (s *Server) checkToken(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AgentToken)) == 1
}

// --- auth rate limiter (spec §8: 5 failures/min/IP -> 60 s ban) -------------

type authLimiter struct {
	mu    sync.Mutex
	state map[string]*failState
}
type failState struct {
	count    int
	window   time.Time
	banUntil time.Time
}

func newAuthLimiter() *authLimiter { return &authLimiter{state: map[string]*failState{}} }

func (l *authLimiter) banned(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state[ip]
	return st != nil && time.Now().Before(st.banUntil)
}

func (l *authLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	st := l.state[ip]
	if st == nil || now.Sub(st.window) > time.Minute {
		st = &failState{window: now}
		l.state[ip] = st
	}
	st.count++
	if st.count >= 5 {
		st.banUntil = now.Add(time.Minute)
		st.count = 0
		st.window = now
	}
}

func (l *authLimiter) ok(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, ip)
}

// newID returns a short random agent id (avoids a uuid dependency).
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
