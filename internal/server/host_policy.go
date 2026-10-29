package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// HostPolicy applies only to the exact canonical HTTP Host, including when its
// agent is disconnected. Unconfigured hosts retain the shared agent limit.
type HostPolicy struct {
	MaxConns             int      `json:"max_conns"`
	HeaderTimeoutSeconds int      `json:"header_timeout_seconds"`
	AllowedOrigins       []string `json:"allowed_origins"`
}

type hostState struct {
	policy HostPolicy
	sem    chan struct{}
	peak   atomic.Int64
	reject atomic.Int64
	errors atomic.Int64
}

func (h *hostState) acquire() (func(), bool) {
	select {
	case h.sem <- struct{}{}:
		n := int64(len(h.sem))
		for peak := h.peak.Load(); n > peak; peak = h.peak.Load() {
			if h.peak.CompareAndSwap(peak, n) {
				break
			}
		}
		return func() { <-h.sem }, true
	default:
		h.reject.Add(1)
		return func() {}, false
	}
}

func (s *Server) publicError(w http.ResponseWriter, r *http.Request, message string, code int) {
	if h := s.hosts[canonHost(r.Host)]; h != nil {
		h.errors.Add(1)
		header := w.Header()
		header.Set("Retry-After", "5")
		header.Set("Cache-Control", "no-store")
		header.Add("Vary", "Origin")
		for _, origin := range h.policy.AllowedOrigins {
			if origin != "" && r.Header.Get("Origin") == origin {
				header.Set("Access-Control-Allow-Origin", origin)
				header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
				header.Set("Access-Control-Allow-Headers", "If-None-Match")
				header.Set("Access-Control-Expose-Headers", "ETag, Retry-After")
				header.Set("Timing-Allow-Origin", origin)
				break
			}
		}
		s.log("host %s forwarder error %d: %s", canonHost(r.Host), code, message)
	}
	http.Error(w, message, code)
}

func gatewayStatus(err error) int {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// yamux's OpenStream has no context API and can wait behind unacknowledged
// SYNs. A timed-out caller leaves no stream behind when that open completes.
func openStream(ctx context.Context, sess *yamux.Session) (*yamux.Stream, error) {
	type result struct {
		st  *yamux.Stream
		err error
	}
	ready := make(chan result)
	go func() {
		st, err := sess.OpenStream()
		select {
		case ready <- result{st, err}:
		case <-ctx.Done():
			if st != nil {
				st.SetDeadline(time.Now())
				st.Close()
			}
		}
	}()
	select {
	case res := <-ready:
		return res.st, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
