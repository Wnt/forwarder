package server

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Wnt/forwarder/internal/framing"
	"github.com/hashicorp/yamux"
)

// agentSession is one connected agent and the tunnels it registered. Multiple
// agents may be connected at once (one per guest project), each owning a
// disjoint set of hostnames/ports — that is what makes the forwarder reusable
// across projects from a single box. A single agent may register several.
type agentSession struct {
	id    string
	sess  *yamux.Session
	hosts map[string]string // hostname -> tunnel id
	ports map[int]string    // remote port -> tunnel id (TCP tunnels)
	lns   []*tcpListener    // public TCP listeners to close on disconnect

	// sem bounds the number of concurrent public connections multiplexed onto
	// this agent's session so one busy (or hostile) guest can't exhaust the box.
	sem chan struct{}
}

func (a *agentSession) acquire() (release func(), ok bool) {
	select {
	case a.sem <- struct{}{}:
		return func() { <-a.sem }, true
	default:
		return func() {}, false
	}
}

// tcpListener pairs a public TCP listener with the tunnel it feeds.
type tcpListener struct {
	ln       net.Listener
	tunnelID string
	port     int
}

// tcpPolicy is the registry's view of the server's TCP config (see Config).
type tcpPolicy struct {
	enabled  bool
	min, max int
	bind     string
	label    string // pretty host for RemoteAddr; falls back to bind
}

// registry is the authoritative host/port -> agent map. On agent disconnect all
// of that agent's entries are removed atomically (removeAgent).
type registry struct {
	mu     sync.RWMutex
	agents map[string]*agentSession
	byHost map[string]*agentSession
	byPort map[int]*agentSession
}

func newRegistry() *registry {
	return &registry{
		agents: map[string]*agentSession{},
		byHost: map[string]*agentSession{},
		byPort: map[int]*agentSession{},
	}
}

// register binds an agent's requested tunnels and returns the per-tunnel verdict
// plus the TCP listeners the caller must start accepting on. Conflicts (a
// hostname/port already owned by another live agent) are rejected per-tunnel; the
// rest still bind. For TCP, the public listener is opened here (under the lock)
// so a bind failure is reported truthfully in the assignment; the accept loop is
// started by the caller after the lock is released. The agent is recorded even if
// it registered zero usable tunnels so removeAgent on disconnect is always safe.
func (r *registry) register(a *agentSession, tunnels []framing.TunnelDef, maxConns int, tp tcpPolicy) ([]framing.TunnelAssigned, []*tcpListener) {
	r.mu.Lock()
	defer r.mu.Unlock()

	a.hosts = map[string]string{}
	a.ports = map[int]string{}
	a.sem = make(chan struct{}, maxConns)
	r.agents[a.id] = a

	out := make([]framing.TunnelAssigned, 0, len(tunnels))
	var toStart []*tcpListener
	for _, t := range tunnels {
		res := framing.TunnelAssigned{ID: t.ID}
		switch t.Proto {
		case framing.ProtoHTTP:
			host := canonHost(t.Hostname)
			if host == "" || t.LocalPort <= 0 {
				res.Error = "http tunnel needs hostname + local_port"
			} else if _, taken := r.byHost[host]; taken {
				res.Error = fmt.Sprintf("hostname %q already registered", host)
			} else {
				r.byHost[host] = a
				a.hosts[host] = t.ID
				res.RemoteAddr = host + ":443"
			}
		case framing.ProtoTCP:
			if tl, err := r.bindTCP(a, t, tp); err != nil {
				res.Error = err.Error()
			} else {
				res.RemoteAddr = fmt.Sprintf("%s:%d", tcpLabel(tp), t.RemotePort)
				toStart = append(toStart, tl)
			}
		default:
			res.Error = fmt.Sprintf("unknown proto %q", t.Proto)
		}
		out = append(out, res)
	}
	return out, toStart
}

// bindTCP validates a TCP tunnel against the policy + current bindings and opens
// its public listener. Caller holds r.mu.
func (r *registry) bindTCP(a *agentSession, t framing.TunnelDef, tp tcpPolicy) (*tcpListener, error) {
	if !tp.enabled {
		return nil, fmt.Errorf("tcp tunnels are not enabled on this forwarder (set FORWARDER_TCP_PORT_RANGE)")
	}
	if t.LocalPort <= 0 {
		return nil, fmt.Errorf("tcp tunnel needs local_port")
	}
	if t.RemotePort < tp.min || t.RemotePort > tp.max {
		return nil, fmt.Errorf("remote_port %d out of allowed range %d-%d", t.RemotePort, tp.min, tp.max)
	}
	if _, taken := r.byPort[t.RemotePort]; taken {
		return nil, fmt.Errorf("remote_port %d already registered", t.RemotePort)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(tp.bind, strconv.Itoa(t.RemotePort)))
	if err != nil {
		return nil, fmt.Errorf("listen :%d: %v", t.RemotePort, err)
	}
	r.byPort[t.RemotePort] = a
	a.ports[t.RemotePort] = t.ID
	tl := &tcpListener{ln: ln, tunnelID: t.ID, port: t.RemotePort}
	a.lns = append(a.lns, tl)
	return tl, nil
}

// removeAgent drops every binding for the agent and closes its TCP listeners
// (which unblocks their accept loops). Safe for an unknown id.
func (r *registry) removeAgent(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agents[id]
	if a == nil {
		return
	}
	for h := range a.hosts {
		if r.byHost[h] == a {
			delete(r.byHost, h)
		}
	}
	for p := range a.ports {
		if r.byPort[p] == a {
			delete(r.byPort, p)
		}
	}
	for _, tl := range a.lns {
		tl.ln.Close()
	}
	a.lns = nil
	delete(r.agents, id)
}

// lookupHost returns the agent serving host (and the tunnel id), or nil.
func (r *registry) lookupHost(host string) (*agentSession, string) {
	host = canonHost(host)
	r.mu.RLock()
	defer r.mu.RUnlock()
	a := r.byHost[host]
	if a == nil {
		return nil, ""
	}
	return a, a.hosts[host]
}

// hostRegistered reports whether any live agent serves host. Used by the
// on-demand-TLS ask guard so Caddy only mints certs for live tunnels.
func (r *registry) hostRegistered(host string) bool {
	a, _ := r.lookupHost(host)
	return a != nil
}

// snapshot returns a stable, sorted view of current bindings for /status.
func (r *registry) snapshot() statusView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v := statusView{Agents: len(r.agents), Hosts: []string{}, TCPPorts: []int{}}
	for h := range r.byHost {
		v.Hosts = append(v.Hosts, h)
	}
	for p := range r.byPort {
		v.TCPPorts = append(v.TCPPorts, p)
	}
	sort.Strings(v.Hosts)
	sort.Ints(v.TCPPorts)
	return v
}

type statusView struct {
	Agents   int      `json:"agents"`
	Hosts    []string `json:"hosts"`
	TCPPorts []int    `json:"tcp_ports"`
}

// tcpLabel is the public host shown in a TCP tunnel's assigned RemoteAddr.
func tcpLabel(tp tcpPolicy) string {
	if tp.label != "" {
		return tp.label
	}
	return tp.bind
}

// canonHost lowercases and strips any :port and trailing dot from a Host/SNI
// value so lookups, registrations and the ask guard all key off the same string.
func canonHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}
