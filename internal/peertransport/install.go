package peertransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// RemotePlatform reports the peer's GOOS-GOARCH pair, such as linux-amd64.
func RemotePlatform(ctx context.Context, endpoint Endpoint) (string, error) {
	if endpoint.IsLocal() {
		return "", errors.New("remote platform detection requires an SSH device")
	}
	endpoint.Binary = "/bin/sh"
	endpoint.Path = ""
	var out bytes.Buffer
	if err := endpoint.Run(ctx, []string{"-c", "uname -s; uname -m"}, nil, &out); err != nil {
		return "", err
	}
	fields := strings.Fields(out.String())
	if len(fields) != 2 {
		return "", fmt.Errorf("unexpected uname output from %s", endpoint.Host)
	}
	return Platform(fields[0], fields[1])
}

// Platform maps uname values onto the names used by release binaries.
func Platform(kernel, machine string) (string, error) {
	var goos, goarch string
	switch strings.ToLower(kernel) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return "", fmt.Errorf("unsupported operating system %s; onthego supports macOS and Linux", kernel)
	}
	switch strings.ToLower(machine) {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "arm64", "aarch64":
		goarch = "arm64"
	default:
		return "", fmt.Errorf("unsupported CPU architecture %s; onthego supports amd64 and arm64", machine)
	}
	return goos + "-" + goarch, nil
}

// installScript writes stdin to ~/.local/bin/onthego atomically, checks that
// the new executable runs, and prints its absolute path.
const installScript = `set -eu
dir="$HOME/.local/bin"
mkdir -p "$dir"
tmp="$dir/.onthego-install.$$"
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
chmod 755 "$tmp"
"$tmp" version >/dev/null
mv -f "$tmp" "$dir/onthego"
trap - EXIT
printf '%s\n' "$dir/onthego"`

// InstallBinary copies an executable to ~/.local/bin/onthego on the peer and
// returns the installed absolute path.
func InstallBinary(ctx context.Context, endpoint Endpoint, binary io.Reader) (string, error) {
	if endpoint.IsLocal() {
		return "", errors.New("install requires an SSH device")
	}
	endpoint.Binary = "/bin/sh"
	endpoint.Path = ""
	var out bytes.Buffer
	if err := endpoint.Run(ctx, []string{"-c", installScript}, binary, &out); err != nil {
		return "", fmt.Errorf("install onthego on %s: %w", endpoint.Host, err)
	}
	installed := strings.TrimSpace(out.String())
	if !filepath.IsAbs(installed) || invalidPath(installed) {
		return "", errors.New("the device did not report an absolute install path")
	}
	return installed, nil
}
