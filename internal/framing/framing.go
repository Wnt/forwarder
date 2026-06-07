// Package framing defines the wire protocol shared by the forwarder server and
// agent: the newline-delimited JSON control messages exchanged on yamux stream 1
// and the per-data-stream connect header. It is deliberately tiny and has no
// dependency on either side's logic so the two binaries can never disagree on
// the wire format.
//
// The protocol is transport-agnostic. In this deployment the yamux session rides
// a WebSocket (so it can reuse the lab's Caddy on :443 and its Let's Encrypt
// cert), but nothing here knows or cares about that — yamux just needs a
// reliable, ordered net.Conn.
package framing

import (
	"bufio"
	"encoding/json"
	"io"
	"time"

	"github.com/hashicorp/yamux"
)

// Protocol version sent in HelloMsg and negotiated as the WebSocket subprotocol.
const (
	Version     = "1"
	Subprotocol = "forwarder.v1"
)

// Message type tags (the "type" field of every control message).
const (
	TypeHello        = "hello"
	TypeHelloAck     = "hello-ack"
	TypeRegister     = "register"
	TypeRegistered   = "registered"
	TypeHeartbeat    = "heartbeat"
	TypeHeartbeatAck = "heartbeat-ack"
)

// Tunnel protocols.
const (
	ProtoHTTP = "http" // routed by Host header, public ingress via Caddy on :443
	ProtoTCP  = "tcp"  // routed by listen port (Phase 2; not enabled in this build)
)

// HelloMsg is the first line the agent writes on the control stream.
type HelloMsg struct {
	Type    string `json:"type"`    // TypeHello
	Token   string `json:"token"`   // shared bearer secret (AGENT_TOKEN)
	Version string `json:"version"` // Version
}

// HelloAckMsg is the server's reply. A non-empty Error means the agent was
// rejected and the session is about to be closed.
type HelloAckMsg struct {
	Type    string `json:"type"` // TypeHelloAck
	AgentID string `json:"agent_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RegisterMsg lists the tunnels the agent wants the server to expose for it.
type RegisterMsg struct {
	Type    string      `json:"type"` // TypeRegister
	Tunnels []TunnelDef `json:"tunnels"`
}

// TunnelDef is one requested tunnel. For ProtoHTTP, Hostname is the public FQDN
// (e.g. ios.lab.madekivi.fi). For ProtoTCP, RemotePort is the public listen port.
type TunnelDef struct {
	ID         string `json:"id"`
	Proto      string `json:"proto"`
	Hostname   string `json:"hostname,omitempty"`
	LocalPort  int    `json:"local_port"`
	RemotePort int    `json:"remote_port,omitempty"`
}

// RegisteredMsg confirms (or rejects, per-tunnel) the registration.
type RegisteredMsg struct {
	Type    string           `json:"type"` // TypeRegistered
	Tunnels []TunnelAssigned `json:"tunnels"`
}

// TunnelAssigned is the server's verdict for one requested tunnel. A non-empty
// Error means that single tunnel was refused (e.g. hostname already taken);
// other tunnels in the same RegisterMsg may still have succeeded.
type TunnelAssigned struct {
	ID         string `json:"id"`
	RemoteAddr string `json:"remote_addr"` // e.g. "ios.lab.madekivi.fi:443"
	Error      string `json:"error,omitempty"`
}

// ConnectHeader is written by the server as the first newline-delimited JSON
// line on each data stream, before the raw proxied bytes. It tells the agent
// which tunnel (hence which local port) the connection belongs to.
type ConnectHeader struct {
	TunnelID   string `json:"id"`
	RemoteAddr string `json:"remote_addr"` // public client IP, best-effort
}

// Heartbeat covers both TypeHeartbeat and TypeHeartbeatAck (same shape). These
// are optional: liveness is primarily handled by yamux's built-in keepalive
// pings (see YamuxConfig). They exist so an application-level ping is available
// if a transport ever swallows yamux pings.
type Heartbeat struct {
	Type string `json:"type"`
}

// WriteJSONLine marshals v and writes it followed by '\n'. Control messages are
// newline-delimited so the reader can frame them with bufio.Reader.ReadBytes.
func WriteJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// ReadJSONLine reads one '\n'-terminated JSON document from r into v.
func ReadJSONLine(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// YamuxConfig returns the session config shared by both ends (Appendix B of the
// spec). The window bump to 2 MB matters: the default 256 KB stalls under the
// continuous JPEG-over-WebSocket stream the first guest (iOS-pwa-runner) pushes.
func YamuxConfig(logOut io.Writer) *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = 256
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 30 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.MaxStreamWindowSize = 2 * 1024 * 1024 // 2 MB
	c.LogOutput = logOut
	return c
}
