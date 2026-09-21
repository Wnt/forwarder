package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/Wnt/forwarder/internal/framing"
	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// handle is the single entry point for everything Caddy reverse-proxies to the
// forwarder's loopback HTTP port. A WebSocket upgrade to the reserved control
// path is the agent dialing in; anything else is public ingress routed by Host
// header to the agent that registered it.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) && r.URL.Path == s.cfg.ControlPath {
		s.handleControl(w, r)
		return
	}
	s.handlePublic(w, r)
}

// handleControl completes the WebSocket handshake, wraps it as a net.Conn, runs
// a yamux server over it, then authenticates and serves the agent.
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	peer := realIP(r)
	if s.limiter.banned(peer) {
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{framing.Subprotocol},
		// Origin enforcement is Caddy's job (this listener is loopback-only);
		// the agent is not a browser and sends no Origin, so don't reject it.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	// yamux frames are small (io.Copy moves <=32 KB chunks), but never let the
	// default 32 KB message cap clip one; bound memory at a safe ceiling instead
	// of disabling the limit entirely on this internet-facing pre-auth conn.
	c.SetReadLimit(4 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)

	sess, err := yamux.Server(nc, framing.YamuxConfig(io.Discard))
	if err != nil {
		c.Close(websocket.StatusInternalError, "yamux")
		return
	}
	defer sess.Close()
	s.serveAgent(sess, peer)
	c.Close(websocket.StatusNormalClosure, "bye")
}

// serveAgent runs the control-stream protocol for one connected agent: auth,
// registration, then a read loop that lasts for the life of the session.
func (s *Server) serveAgent(sess *yamux.Session, peer string) {
	// The agent opens exactly one stream — the control stream — so the server
	// only ever accepts once. All other streams are server-initiated (data).
	ctl, err := sess.AcceptStream()
	if err != nil {
		return
	}
	br := bufio.NewReader(ctl)

	var hello framing.HelloMsg
	if err := framing.ReadJSONLine(br, &hello); err != nil {
		return
	}
	if hello.Type != framing.TypeHello || !s.checkToken(hello.Token) {
		_ = framing.WriteJSONLine(ctl, framing.HelloAckMsg{Type: framing.TypeHelloAck, Error: "unauthorized"})
		s.limiter.fail(peer)
		s.log("agent auth REJECTED from %s", peer)
		return
	}
	s.limiter.ok(peer)

	as := &agentSession{id: newID(), sess: sess}
	_ = framing.WriteJSONLine(ctl, framing.HelloAckMsg{Type: framing.TypeHelloAck, AgentID: as.id})

	var reg framing.RegisterMsg
	if err := framing.ReadJSONLine(br, &reg); err != nil {
		return
	}
	assigned, tcpListeners := s.reg.register(as, reg.Tunnels, s.cfg.MaxConnsPerTunnel, s.cfg.tcpPolicy())
	_ = framing.WriteJSONLine(ctl, framing.RegisteredMsg{Type: framing.TypeRegistered, Tunnels: assigned})
	defer s.reg.removeAgent(as.id)

	// Accept loops for the public TCP listeners opened during register. They exit
	// when removeAgent closes the listeners on disconnect.
	for _, tl := range tcpListeners {
		go s.acceptTCP(as, tl)
	}

	for _, t := range assigned {
		if t.Error != "" {
			s.log("agent %s tunnel %q rejected: %s", as.id, t.ID, t.Error)
		} else {
			s.log("agent %s tunnel %q -> %s", as.id, t.ID, t.RemoteAddr)
		}
	}
	s.log("agent %s registered (%d tunnels) from %s", as.id, len(assigned), peer)

	// Control-stream read loop: answer optional heartbeats and, more importantly,
	// block until the stream errors — that is how we learn the agent is gone and
	// trigger the deferred removeAgent. yamux keepalive detects dead transports.
	for {
		var m framing.Heartbeat
		if err := framing.ReadJSONLine(br, &m); err != nil {
			s.log("agent %s disconnected", as.id)
			return
		}
		if m.Type == framing.TypeHeartbeat {
			_ = framing.WriteJSONLine(ctl, framing.Heartbeat{Type: framing.TypeHeartbeatAck})
		}
	}
}

// handlePublic routes one public request to the agent that registered its Host,
// then proxies it transparently — per-request for plain HTTP (so Caddy's backend
// keepalive pool stays correct), raw-piped for WebSocket upgrades (so the JPEG
// stream is never buffered or re-framed).
func (s *Server) handlePublic(w http.ResponseWriter, r *http.Request) {
	as, tunnelID := s.reg.lookupHost(r.Host)
	if as == nil {
		http.Error(w, "no tunnel registered for "+canonHost(r.Host), http.StatusBadGateway)
		return
	}
	release, ok := as.acquire()
	if !ok {
		http.Error(w, "tunnel at connection limit", http.StatusServiceUnavailable)
		return
	}
	defer release()

	st, err := as.sess.OpenStream()
	if err != nil {
		http.Error(w, "agent unavailable", http.StatusBadGateway)
		return
	}
	defer st.Close()

	hdr := framing.ConnectHeader{TunnelID: tunnelID, RemoteAddr: realIP(r)}
	if err := framing.WriteJSONLine(st, hdr); err != nil {
		http.Error(w, "agent write error", http.StatusBadGateway)
		return
	}
	setForwardingHeaders(r)

	if isWebSocketUpgrade(r) {
		proxyWS(w, r, st)
		return
	}
	proxyHTTP(w, r, st)
}

// proxyHTTP forwards a single (non-upgrade) request/response over the stream.
func proxyHTTP(w http.ResponseWriter, r *http.Request, st *yamux.Stream) {
	// Re-serialize the request to the agent in origin form. r.Write preserves the
	// original Host and writes the body, which is exactly what the runner behind
	// the agent expects.
	if err := r.Write(st); err != nil {
		http.Error(w, "agent write error", http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(st), r)
	if err != nil {
		http.Error(w, "agent read error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	// Flush as we go so streaming responses (SSE, chunked) reach the client live.
	io.Copy(flushWriter{w}, resp.Body)
}

// proxyWS hijacks the client connection and raw-pipes it to the stream after
// writing the upgrade request. From here the forwarder is a byte relay: it never
// parses a WebSocket frame, so binary JPEG payloads pass through unbuffered and
// without any payload-size cap.
func proxyWS(w http.ResponseWriter, r *http.Request, st *yamux.Stream) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	client, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	// Send the upgrade request itself; the runner's 101 (and every frame after)
	// flows back over the stream and out to the client untouched.
	if err := r.Write(st); err != nil {
		return
	}
	// bufrw.Reader for the client->agent direction: Hijack may have buffered
	// bytes past the request that a raw client.Read would miss.
	pipe(st, client, bufrw.Reader)
}

// acceptTCP serves one public TCP tunnel listener: every accepted connection is
// spliced to the agent over a fresh yamux stream. Returns when the listener is
// closed (removeAgent on disconnect).
func (s *Server) acceptTCP(as *agentSession, tl *tcpListener) {
	s.log("agent %s tcp tunnel %q listening on %s", as.id, tl.tunnelID, tl.ln.Addr())
	for {
		c, err := tl.ln.Accept()
		if err != nil {
			return
		}
		go s.proxyTCP(as, c, tl.tunnelID)
	}
}

// proxyTCP relays one raw TCP connection to the agent. Unlike HTTP/WS there is no
// header to parse: write the ConnectHeader, then splice bytes both ways.
func (s *Server) proxyTCP(as *agentSession, client net.Conn, tunnelID string) {
	defer client.Close()
	release, ok := as.acquire()
	if !ok {
		return
	}
	defer release()
	st, err := as.sess.OpenStream()
	if err != nil {
		return
	}
	defer st.Close()
	hdr := framing.ConnectHeader{TunnelID: tunnelID, RemoteAddr: client.RemoteAddr().String()}
	if err := framing.WriteJSONLine(st, hdr); err != nil {
		return
	}
	pipe(st, client, client)
}

// handleAsk backs Caddy's on_demand_tls { ask ... }. Caddy issues a cert for an
// SNI only if this returns 2xx, so we approve exactly the control host and any
// hostname a live agent has registered — never an open relay, never a
// rate-limit-burning cert for an arbitrary SNI.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	domain := canonHost(r.URL.Query().Get("domain"))
	if domain == "" {
		http.Error(w, "missing domain", http.StatusBadRequest)
		return
	}
	if domain == s.cfg.ControlHost || s.reg.hostRegistered(domain) {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "unknown domain", http.StatusForbidden)
}

// --- shared helpers ---------------------------------------------------------

// pipe copies bytes both ways between a yamux stream and a hijacked client conn
// and returns when EITHER direction ends, then closes both. Full (not half)
// close is deliberate: a WebSocket is bidirectional and ends together, and it
// avoids yamux's half-closed StreamCloseTimeout force-killing a still-streaming
// direction (the JPEG case, where the client sends almost nothing).
func pipe(st *yamux.Stream, client net.Conn, clientR io.Reader) {
	var once sync.Once
	stop := func() { once.Do(func() { st.Close(); client.Close() }) }
	go func() {
		io.Copy(st, clientR) // client -> agent
		stop()
	}()
	io.Copy(client, st) // agent -> client
	stop()
}

func isWebSocketUpgrade(r *http.Request) bool {
	return tokenInHeader(r.Header, "Connection", "upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

func tokenInHeader(h http.Header, key, want string) bool {
	for _, v := range h[key] {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), want) {
				return true
			}
		}
	}
	return false
}

// setForwardingHeaders REPLACES the forwarding headers with what this hop can
// actually vouch for, rather than passing on what arrived.
//
// It used to fill X-Forwarded-For "only if empty, so we never clobber the true
// client IP Caddy already recorded". Two things were wrong with that. Caddy
// APPENDS to whatever the client sent, so a non-empty header is not evidence of
// truth — it is `<whatever the client made up>, <the address Caddy saw>`. And a
// downstream reader taking the first hop (the conventional choice) would read
// the client's own invention. Anything trusting this header for rate limiting,
// geography or an audit trail was therefore trusting the caller.
//
// So: collapse it to the ONE value realIP vouches for, and overwrite. Callers
// downstream get a single-valued header they can use without parsing a trust
// boundary they cannot see from where they sit.
func setForwardingHeaders(r *http.Request) {
	r.Header.Set("X-Forwarded-Proto", "https")
	if ip := realIP(r); ip != "" {
		r.Header.Set("X-Forwarded-For", ip)
		r.Header.Set("X-Real-IP", ip)
	}
}

// realIP is the client address this hop is willing to assert.
//
// THE TRUST BOUNDARY. Our own Caddy is the TLS terminator immediately in front
// and reaches us over loopback, and it appends the address it saw to any
// X-Forwarded-For the client supplied. So when the peer is loopback the LAST
// entry is Caddy's own observation and every earlier entry is the client
// talking about itself; we take the last and discard the rest. When the peer is
// NOT loopback the request did not come through Caddy, so the header carries no
// weight at all and only the socket peer is real.
//
// This is what makes the header trustworthy downstream: past this point
// X-Forwarded-For is single-valued and was written by a hop that could see the
// connection, not by whoever opened it.
func realIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !isLoopback(peer) {
		return peer
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if last != "" {
			return last
		}
	}
	return peer
}

// isLoopback reports whether addr is this box talking to itself — the only
// position from which our own Caddy can hand us an address to believe.
func isLoopback(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// flushWriter flushes after every write so proxied streaming bodies aren't held.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
