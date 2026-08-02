package agent

import (
	"testing"

	"github.com/Wnt/forwarder/internal/framing"
)

func TestParseTunnels(t *testing.T) {
	defs, err := ParseTunnels("web:http:ios.lab.madekivi.fi:8080, ssh:tcp:10022:22")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d tunnels", len(defs))
	}
	if defs[0].ID != "web" || defs[0].Proto != framing.ProtoHTTP || defs[0].Hostname != "ios.lab.madekivi.fi" || defs[0].LocalPort != 8080 {
		t.Fatalf("tunnel[0] = %+v", defs[0])
	}
	if defs[1].ID != "ssh" || defs[1].Proto != framing.ProtoTCP || defs[1].RemotePort != 10022 || defs[1].LocalPort != 22 {
		t.Fatalf("tunnel[1] = %+v", defs[1])
	}

	for _, bad := range []string{
		"",              // empty
		"web:http:host", // too few fields
		"web:http:host:notaport",
		"x:bogus:host:1",    // unknown proto
		"x:tcp:notaport:22", // bad remote port
		"x:http::8080",      // http needs hostname
	} {
		if _, err := ParseTunnels(bad); err == nil {
			t.Errorf("ParseTunnels(%q) should error", bad)
		}
	}
}

func TestControlURL(t *testing.T) {
	cases := map[string]string{
		"tunnel.lab.madekivi.fi":                              "wss://tunnel.lab.madekivi.fi/__forwarder/v1/control",
		"wss://tunnel.lab.madekivi.fi/__forwarder/v1/control": "wss://tunnel.lab.madekivi.fi/__forwarder/v1/control",
		"ws://localhost:7080/__forwarder/v1/control":          "ws://localhost:7080/__forwarder/v1/control",
	}
	for in, want := range cases {
		if got := ControlURL(in); got != want {
			t.Errorf("ControlURL(%q) = %q, want %q", in, got, want)
		}
	}
}
