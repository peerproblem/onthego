package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/peertransport"
)

func endpoint(d config.Device) peertransport.Endpoint {
	return peertransport.Endpoint{Host: d.Host, Path: devicePath(d), Port: d.Port, IdentityFile: d.IdentityFile, Binary: d.Binary}
}
func devicePath(d config.Device) string {
	if d.LocalPath != "" {
		return d.LocalPath
	}
	return d.Path
}
func device(name string) (config.Device, error) {
	u, err := config.LoadUser()
	if err != nil {
		return config.Device{}, err
	}
	d, ok := u.Devices[name]
	if !ok {
		return d, fmt.Errorf("device %q is not configured; run onthego device add", name)
	}
	if err = config.ValidateDevice(d); err != nil {
		return d, err
	}
	if d.AgeRecipient == "" || d.SigningPublic == "" {
		return d, errors.New("device public keys are missing; remove and add this device again")
	}
	return d, nil
}
func peerJSON(ctx context.Context, d config.Device, args []string, input io.Reader, result any) error {
	var b bytes.Buffer
	if err := endpoint(d).Run(ctx, append([]string{"--json"}, args...), input, &b); err != nil {
		return err
	}
	if err := json.Unmarshal(b.Bytes(), result); err != nil {
		return fmt.Errorf("invalid peer response (install the same onthego version on both machines): %w", err)
	}
	return nil
}

const protocolMajor = 3

var protocolCapabilities = []string{"git-wip", "incremental-bundle", "mutual-trust", "session-policy", "idempotent-apply"}

type helloResponse struct {
	ProtocolMajor int      `json:"protocol_major"`
	Version       string   `json:"version"`
	Capabilities  []string `json:"capabilities"`
	Home          string   `json:"home"`
}

func negotiatePeer(ctx context.Context, d config.Device) (helloResponse, error) {
	var hello helloResponse
	if err := peerJSON(ctx, d, []string{"peer", "hello"}, nil, &hello); err != nil {
		return hello, err
	}
	if hello.ProtocolMajor != protocolMajor {
		return hello, fmt.Errorf("peer protocol %d is incompatible with local protocol %d; update onthego on both machines", hello.ProtocolMajor, protocolMajor)
	}
	for _, required := range protocolCapabilities {
		found := false
		for _, capability := range hello.Capabilities {
			if capability == required {
				found = true
				break
			}
		}
		if !found {
			return hello, fmt.Errorf("peer lacks %s; update onthego on the peer", required)
		}
	}
	return hello, nil
}
func identityName(public string) string {
	sum := sha256.Sum256([]byte(public))
	return "device-" + hex.EncodeToString(sum[:16])
}
func resolveDevicePath(name string, d config.Device, root string, cfg config.Config) (config.Device, error) {
	peers, err := config.LoadProjectPeers(cfg.ProjectID)
	if err != nil {
		return d, err
	}
	target := peers.Peers[name]
	if target == "" {
		target = devicePath(d)
	} // migrate earlier private configs lazily
	if target == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return d, err
		}
		relative, err := filepath.Rel(home, root)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return d, errors.New("project is outside HOME; set its destination with onthego remote set " + name + " /absolute/path")
		}
		if d.Home == "" {
			return d, errors.New("peer home is missing; pair the device again")
		}
		target = filepath.Join(d.Home, relative)
	}
	if d.Local || d.LocalPath != "" {
		d.LocalPath = target
	} else {
		d.Path = target
	}
	return d, nil
}
