// Package tailscale reads the local Tailscale client state so devices on the
// user's tailnet can be paired by name. ONTHEGO never changes Tailscale
// settings; it only runs `tailscale status --json`.
package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/peerproblem/onthego/internal/otgerror"
)

// macOSApp is where the Mac App Store and standalone apps keep the CLI when it
// has not been added to PATH.
const macOSApp = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// Peer is one device on the tailnet, including this device as Self.
type Peer struct {
	HostName     string   `json:"host_name"`
	DNSName      string   `json:"dns_name,omitempty"`
	OS           string   `json:"os,omitempty"`
	TailscaleIPs []string `json:"tailscale_ips,omitempty"`
	Online       bool     `json:"online"`
}

// Status is the subset of `tailscale status --json` that pairing needs.
type Status struct {
	BackendState string `json:"backend_state"`
	Tailnet      string `json:"tailnet,omitempty"`
	Self         Peer   `json:"self"`
	Peers        []Peer `json:"peers"`
}

type rawPeer struct {
	HostName     string
	DNSName      string
	OS           string
	TailscaleIPs []string
	Online       bool
}

type rawStatus struct {
	BackendState   string
	Self           *rawPeer
	Peer           map[string]*rawPeer
	MagicDNSSuffix string
	CurrentTailnet *struct {
		Name           string
		MagicDNSSuffix string
	}
}

// ErrNotInstalled means no tailscale CLI was found on this device.
var ErrNotInstalled = errors.New("tailscale CLI not found")

// Find returns the tailscale CLI path.
func Find() (string, error) {
	if path, err := exec.LookPath("tailscale"); err == nil {
		return path, nil
	}
	if runtime.GOOS == "darwin" {
		if info, err := os.Stat(macOSApp); err == nil && !info.IsDir() {
			return macOSApp, nil
		}
	}
	return "", ErrNotInstalled
}

// Read runs the local tailscale CLI and parses its status.
func Read(ctx context.Context) (Status, error) {
	path, err := Find()
	if err != nil {
		return Status{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "status", "--json").Output()
	if err != nil && len(out) == 0 {
		return Status{}, fmt.Errorf("tailscale status failed: %w", err)
	}
	return Parse(out)
}

// Parse converts `tailscale status --json` output.
func Parse(data []byte) (Status, error) {
	var raw rawStatus
	if err := json.Unmarshal(data, &raw); err != nil {
		return Status{}, fmt.Errorf("unexpected tailscale status output: %w", err)
	}
	st := Status{BackendState: raw.BackendState, Tailnet: raw.MagicDNSSuffix}
	if raw.CurrentTailnet != nil {
		if raw.CurrentTailnet.MagicDNSSuffix != "" {
			st.Tailnet = raw.CurrentTailnet.MagicDNSSuffix
		} else if raw.CurrentTailnet.Name != "" {
			st.Tailnet = raw.CurrentTailnet.Name
		}
	}
	convert := func(p *rawPeer) Peer {
		return Peer{HostName: p.HostName, DNSName: strings.TrimSuffix(p.DNSName, "."), OS: p.OS, TailscaleIPs: p.TailscaleIPs, Online: p.Online}
	}
	if raw.Self != nil {
		st.Self = convert(raw.Self)
		st.Self.Online = true
	}
	for _, p := range raw.Peer {
		if p != nil {
			st.Peers = append(st.Peers, convert(p))
		}
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].Name() < st.Peers[j].Name() })
	return st, nil
}

// Running reports whether this device is logged in and connected.
func (s Status) Running() bool { return s.BackendState == "Running" }

// Name is the short MagicDNS name, falling back to the OS host name.
func (p Peer) Name() string {
	if p.DNSName != "" {
		short, _, _ := strings.Cut(p.DNSName, ".")
		return strings.ToLower(short)
	}
	return strings.ToLower(p.HostName)
}

// Address is what SSH should dial: the MagicDNS name when available, otherwise
// the first Tailscale IP.
func (p Peer) Address() string {
	if p.DNSName != "" {
		return p.DNSName
	}
	if len(p.TailscaleIPs) > 0 {
		return p.TailscaleIPs[0]
	}
	return p.HostName
}

// Resolve finds a peer by short name, full MagicDNS name, host name, or IP.
func (s Status) Resolve(name string) (Peer, error) {
	want := strings.ToLower(strings.TrimSuffix(name, "."))
	var matches []Peer
	for _, p := range s.Peers {
		if p.Name() == want || strings.ToLower(p.DNSName) == want || strings.ToLower(p.HostName) == want || contains(p.TailscaleIPs, want) {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 0:
		return Peer{}, otgerror.New(otgerror.CodeInput, fmt.Sprintf("no device named %q on your tailnet; run onthego tailscale to list devices", name))
	case 1:
		if !matches[0].Online {
			return matches[0], otgerror.New(otgerror.CodeTargetUnavailable, fmt.Sprintf("%s is offline on your tailnet; wake it or start Tailscale there", matches[0].Name()))
		}
		return matches[0], nil
	}
	return Peer{}, otgerror.New(otgerror.CodeInput, fmt.Sprintf("%q matches several tailnet devices; use the full MagicDNS name", name))
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
