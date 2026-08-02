// Package agent implements the guest-side forwarder: it dials out to the box
// over wss, authenticates, registers its tunnels, and forwards each public
// connection the server pushes to a local port. It is application-agnostic.
package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wnt/forwarder/internal/framing"
	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// Config configures Run.
type Config struct {
	ServerURL string              // ws(s):// control URL (see ControlURL)
	Token     string              // shared bearer secret
	Tunnels   []framing.TunnelDef // tunnels to register
	Insecure  bool                // skip TLS verification (dev only)
	Logf      func(string, ...any)
}

func (c *Config) logf(f string, a ...any) {
	if c.Logf != nil {
		c.Logf(f, a...)
	}
}

func (c *Config) localPort() map[string]int {
	m := map[string]int{}
	for _, t := range c.Tunnels {
		m[t.ID] = t.LocalPort
	}
	return m
}

// Run is the supervised reconnect loop. It returns only when ctx is cancelled.
// Backoff doubles to a 60 s cap with jitter and resets to 1 s after a session
// that lasted long enough to be considered healthy — so a brief box restart
// doesn't permanently slow reconnects.
func Run(ctx context.Context, cfg Config) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := connectAndServe(ctx, cfg)
		dur := time.Since(start)
		if ctx.Err() != nil {
			break
		}
		if dur > 60*time.Second {
			backoff = time.Second
		}
		jitter := time.Duration(rand.Int63n(int64(backoff/10) + 1))
		cfg.logf("session ended after %s: %v; reconnecting in %s", dur.Round(time.Second), err, (backoff + jitter).Round(time.Millisecond))
		select {
		case <-time.After(backoff + jitter):
		case <-ctx.Done():
		}
		if backoff < 60*time.Second {
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
		}
	}
}

func connectAndServe(ctx context.Context, cfg Config) error {
	dialOpts := &websocket.DialOptions{Subprotocols: []string{framing.Subprotocol}}
	if cfg.Insecure {
		dialOpts.HTTPClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
	}
	c, _, err := websocket.Dial(ctx, cfg.ServerURL, dialOpts)
	if err != nil {
		return fmt.Errorf("dial %s: %w", cfg.ServerURL, err)
	}
	c.SetReadLimit(4 << 20)
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	nc := websocket.NetConn(connCtx, c, websocket.MessageBinary)
	defer c.Close(websocket.StatusNormalClosure, "bye")

	sess, err := yamux.Client(nc, framing.YamuxConfig(io.Discard))
	if err != nil {
		return fmt.Errorf("yamux: %w", err)
	}
	defer sess.Close()

	// The agent opens the single control stream; data streams are accepted below.
	ctl, err := sess.OpenStream()
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	br := bufio.NewReader(ctl)

	if err := framing.WriteJSONLine(ctl, framing.HelloMsg{Type: framing.TypeHello, Token: cfg.Token, Version: framing.Version}); err != nil {
		return err
	}
	var ack framing.HelloAckMsg
	if err := framing.ReadJSONLine(br, &ack); err != nil {
		return fmt.Errorf("read hello-ack: %w", err)
	}
	if ack.Error != "" {
		return fmt.Errorf("auth rejected: %s", ack.Error)
	}
	cfg.logf("authenticated (agent_id=%s)", ack.AgentID)

	if err := framing.WriteJSONLine(ctl, framing.RegisterMsg{Type: framing.TypeRegister, Tunnels: cfg.Tunnels}); err != nil {
		return err
	}
	var regd framing.RegisteredMsg
	if err := framing.ReadJSONLine(br, &regd); err != nil {
		return fmt.Errorf("read registered: %w", err)
	}
	for _, t := range regd.Tunnels {
		if t.Error != "" {
			cfg.logf("tunnel %q REJECTED: %s", t.ID, t.Error)
		} else {
			cfg.logf("tunnel %q live at https://%s", t.ID, strings.TrimSuffix(t.RemoteAddr, ":443"))
		}
	}

	// Drain the control stream so a server heartbeat or close is observed; its
	// error cancels the data-stream loop via connCtx.
	go func() {
		defer cancel()
		var m framing.Heartbeat
		for {
			if err := framing.ReadJSONLine(br, &m); err != nil {
				return
			}
		}
	}()

	ports := cfg.localPort()
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return fmt.Errorf("accept stream: %w", err)
		}
		go handleDataStream(cfg, ports, st)
	}
}

// handleDataStream reads the ConnectHeader, dials the matching local port, and
// raw-pipes the two together. The bufio.Reader is critical: the bytes after the
// header line (the actual HTTP request / WS upgrade) are already buffered in it.
func handleDataStream(cfg Config, ports map[string]int, st *yamux.Stream) {
	defer st.Close()
	br := bufio.NewReader(st)
	var h framing.ConnectHeader
	if err := framing.ReadJSONLine(br, &h); err != nil {
		return
	}
	port, ok := ports[h.TunnelID]
	if !ok {
		cfg.logf("data stream for unknown tunnel %q", h.TunnelID)
		return
	}
	local, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		cfg.logf("dial 127.0.0.1:%d: %v", port, err)
		return
	}
	defer local.Close()

	var once sync.Once
	stop := func() { once.Do(func() { st.Close(); local.Close() }) }
	go func() {
		io.Copy(st, local) // local -> server
		stop()
	}()
	io.Copy(local, br) // server -> local (drains buffered header remainder first)
	stop()
}

// ParseTunnels parses comma-separated tunnel specs:
//
//	id:http:hostname:localport     e.g. web:http:ios.lab.madekivi.fi:8080
//	id:tcp:remoteport:localport    e.g. ssh:tcp:10022:22
func ParseTunnels(spec string) ([]framing.TunnelDef, error) {
	var out []framing.TunnelDef
	for _, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, ":")
		if len(parts) != 4 {
			return nil, fmt.Errorf("%q: want id:proto:hostname-or-remoteport:localport", raw)
		}
		lp, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil, fmt.Errorf("%q: bad local port %q", raw, parts[3])
		}
		switch parts[1] {
		case framing.ProtoHTTP:
			if parts[2] == "" {
				return nil, fmt.Errorf("%q: http tunnel needs a hostname", raw)
			}
			out = append(out, framing.TunnelDef{ID: parts[0], Proto: framing.ProtoHTTP, Hostname: parts[2], LocalPort: lp})
		case framing.ProtoTCP:
			rp, err := strconv.Atoi(parts[2])
			if err != nil {
				return nil, fmt.Errorf("%q: bad remote port %q", raw, parts[2])
			}
			out = append(out, framing.TunnelDef{ID: parts[0], Proto: framing.ProtoTCP, RemotePort: rp, LocalPort: lp})
		default:
			return nil, fmt.Errorf("%q: unknown proto %q (want http or tcp)", raw, parts[1])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tunnels parsed")
	}
	return out, nil
}

// ControlURL accepts a full ws(s):// URL or a bare host[:port] and returns the
// control endpoint URL. A bare host defaults to wss + the standard control path.
func ControlURL(s string) string {
	if strings.HasPrefix(s, "ws://") || strings.HasPrefix(s, "wss://") {
		return s
	}
	return "wss://" + s + "/__forwarder/v1/control"
}
