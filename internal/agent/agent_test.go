package agent

import "testing"

func TestParseTunnels(t *testing.T) {
	defs, err := ParseTunnels("web:http:ios.lab.madekivi.fi:8080, api:http:api.lab.madekivi.fi:9090")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d tunnels", len(defs))
	}
	if defs[0].ID != "web" || defs[0].Hostname != "ios.lab.madekivi.fi" || defs[0].LocalPort != 8080 {
		t.Fatalf("tunnel[0] = %+v", defs[0])
	}

	for _, bad := range []string{"", "web:http:host", "web:tcp:2222:22", "web:http:host:notaport"} {
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
