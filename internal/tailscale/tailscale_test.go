package tailscale

import (
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/otgerror"
)

const fixture = `{
  "Version": "1.90.0",
  "BackendState": "Running",
  "Self": {"HostName": "Desktop", "DNSName": "desktop.tail1234.ts.net.", "OS": "macOS", "TailscaleIPs": ["100.64.0.1"], "Online": false},
  "MagicDNSSuffix": "tail1234.ts.net",
  "CurrentTailnet": {"Name": "me@example.com", "MagicDNSSuffix": "tail1234.ts.net", "MagicDNSEnabled": true},
  "Peer": {
    "nodekey:1": {"HostName": "MacBook-Pro", "DNSName": "laptop.tail1234.ts.net.", "OS": "macOS", "TailscaleIPs": ["100.64.0.2"], "Online": true},
    "nodekey:2": {"HostName": "vps", "DNSName": "vps.tail1234.ts.net.", "OS": "linux", "TailscaleIPs": ["100.64.0.3"], "Online": false},
    "nodekey:3": {"HostName": "nodns", "DNSName": "", "OS": "linux", "TailscaleIPs": ["100.64.0.4"], "Online": true}
  }
}`

func TestParseAndResolve(t *testing.T) {
	st, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running() || st.Tailnet != "tail1234.ts.net" || st.Self.Name() != "desktop" || !st.Self.Online {
		t.Fatalf("unexpected status: %+v", st)
	}
	if len(st.Peers) != 3 || st.Peers[0].Name() != "laptop" {
		t.Fatalf("peers not sorted by name: %+v", st.Peers)
	}
	for _, name := range []string{"laptop", "LAPTOP", "laptop.tail1234.ts.net", "laptop.tail1234.ts.net.", "macbook-pro", "100.64.0.2"} {
		p, err := st.Resolve(name)
		if err != nil || p.Address() != "laptop.tail1234.ts.net" {
			t.Fatalf("resolve %s: %+v %v", name, p, err)
		}
	}
	if p, err := st.Resolve("nodns"); err != nil || p.Address() != "100.64.0.4" {
		t.Fatalf("IP fallback: %+v %v", p, err)
	}
	if _, err := st.Resolve("vps"); err == nil || otgerror.Public(err).Code != otgerror.CodeTargetUnavailable || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline peer accepted: %v", err)
	}
	if _, err := st.Resolve("missing"); err == nil || otgerror.Public(err).Code != otgerror.CodeInput {
		t.Fatalf("missing peer accepted: %v", err)
	}
}

func TestParseLoggedOut(t *testing.T) {
	st, err := Parse([]byte(`{"BackendState":"NeedsLogin","Self":null,"Peer":null}`))
	if err != nil || st.Running() || len(st.Peers) != 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := Parse([]byte("not json")); err == nil {
		t.Fatal("invalid output accepted")
	}
}
