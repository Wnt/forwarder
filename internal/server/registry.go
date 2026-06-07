package server

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Wnt/stream-connect/lab/forwarder/internal/framing"
	"github.com/hashicorp/yamux"
)

// agentSession is one connected agent and the tunnels it registered. Multiple
// agents may be connected at once (one per guest project), each owning a
// disjoint set of hostnames — that is what makes the forwarder reusable across
// projects from a single box. A single agent may register several hostnames.
type agentSession struct {
	id    string
	sess  *yamux.Session
	hosts map[string]string // hostname -> tunnel id
	ports map[int]string    // remote port -> tunnel id (Phase 2)

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

// register binds an agent's requested tunnels and returns the per-tunnel
// verdict. Conflicts (a hostname already owned by another live agent) are
// rejected per-tunnel; the rest still bind. The agent is recorded even if it
// registered zero usable tunnels so removeAgent on disconnect is always safe.
func (r *registry) register(a *agentSession, tunnels []framing.TunnelDef, maxConns int) []framing.TunnelAssigned {
	r.mu.Lock()
	defer r.mu.Unlock()

	a.hosts = map[string]string{}
	a.ports = map[int]string{}
	a.sem = make(chan struct{}, maxConns)
	r.agents[a.id] = a

	out := make([]framing.TunnelAssigned, 0, len(tunnels))
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
			// Phase 2. Raw TCP can't ride Caddy's HTTP front door, so it would
			// need a dedicated listen port + an nftables hole — out of scope for
			// the "reuse the box, no new firewall surface" Phase 1. The framing
			// carries it so agents/protocol stay forward-compatible.
			res.Error = "tcp tunnels are not enabled on this forwarder (Phase 2)"
		default:
			res.Error = fmt.Sprintf("unknown proto %q", t.Proto)
		}
		out = append(out, res)
	}
	return out
}

// removeAgent drops every binding for the agent. Safe for an unknown id.
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
	v := statusView{Agents: len(r.agents), Hosts: []string{}}
	for h := range r.byHost {
		v.Hosts = append(v.Hosts, h)
	}
	sort.Strings(v.Hosts)
	return v
}

type statusView struct {
	Agents int      `json:"agents"`
	Hosts  []string `json:"hosts"`
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
