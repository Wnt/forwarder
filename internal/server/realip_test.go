package server

import (
	"net/http"
	"testing"
)

// The header this hop emits is the one everything downstream rate-limits,
// geolocates and audits on. These cases pin the trust boundary: an address is
// believed only when a hop that could SEE the connection wrote it.
func TestRealIPTrustBoundary(t *testing.T) {
	cases := []struct {
		name   string
		peer   string
		xff    string
		want   string
		reason string
	}{
		{
			name:   "caddy on loopback: take Caddy's own observation, not the client's claim",
			peer:   "127.0.0.1:44310",
			xff:    "203.0.113.9, 198.51.100.7",
			want:   "198.51.100.7",
			reason: "Caddy APPENDS the address it saw, so the last entry is the only one it vouches for",
		},
		{
			name:   "spoof attempt through Caddy is discarded, not merely deprioritised",
			peer:   "127.0.0.1:44310",
			xff:    "10.0.0.1, 10.0.0.2, 198.51.100.7",
			want:   "198.51.100.7",
			reason: "every entry before Caddy's is the caller talking about itself",
		},
		{
			name:   "loopback peer, no header: the peer is all there is",
			peer:   "127.0.0.1:44310",
			xff:    "",
			want:   "127.0.0.1",
			reason: "nothing to believe, so assert only what the socket shows",
		},
		{
			name:   "peer is NOT loopback: the header did not come through Caddy and carries no weight",
			peer:   "198.51.100.200:5000",
			xff:    "203.0.113.9",
			want:   "198.51.100.200",
			reason: "a direct caller must not be able to name itself",
		},
		{
			name:   "IPv6 loopback is loopback",
			peer:   "[::1]:44310",
			xff:    "203.0.113.9, 198.51.100.7",
			want:   "198.51.100.7",
			reason: "the trust boundary is the position, not the address family",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}, RemoteAddr: c.peer}
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := realIP(r); got != c.want {
				t.Fatalf("realIP(peer=%q, xff=%q) = %q, want %q — %s", c.peer, c.xff, got, c.want, c.reason)
			}
		})
	}
}

// Downstream readers use the conventional "first hop" rule. That is only safe
// if this hop leaves exactly one hop to read, so the overwrite is the contract.
func TestSetForwardingHeadersCollapsesToOneValue(t *testing.T) {
	r := &http.Request{Header: http.Header{}, RemoteAddr: "127.0.0.1:44310"}
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.7")

	setForwardingHeaders(r)

	if got := r.Header.Get("X-Forwarded-For"); got != "198.51.100.7" {
		t.Fatalf("X-Forwarded-For = %q, want the single trusted value %q", got, "198.51.100.7")
	}
	if got := r.Header.Get("X-Real-IP"); got != "198.51.100.7" {
		t.Fatalf("X-Real-IP = %q, want %q", got, "198.51.100.7")
	}
	if got := r.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want https", got)
	}
}

// A client-supplied header must never survive when the caller is not behind our
// own terminator — this is the case that made the old "set only if empty"
// behaviour unsafe.
func TestSetForwardingHeadersIgnoresHeaderFromDirectCaller(t *testing.T) {
	r := &http.Request{Header: http.Header{}, RemoteAddr: "198.51.100.200:5000"}
	r.Header.Set("X-Forwarded-For", "203.0.113.9")

	setForwardingHeaders(r)

	if got := r.Header.Get("X-Forwarded-For"); got != "198.51.100.200" {
		t.Fatalf("X-Forwarded-For = %q, want the socket peer %q", got, "198.51.100.200")
	}
}
