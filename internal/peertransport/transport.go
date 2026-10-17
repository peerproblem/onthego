// Package peertransport runs ONTHEGO directly on a user-owned peer. SSH uses
// the user's existing SSH configuration, credentials, and known-hosts file;
// it does not need a central service or an API key.
package peertransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/otgerror"
)

const DefaultBinary = "onthego"

// Endpoint describes one machine. An empty Host runs Binary on
// this machine, which also permits testing transfers between separate homes.
// Path is an optional absolute project root; callers pass it explicitly to
// the peer command as --root. IdentityFile refers to a local SSH private key.
// Env contains local-only KEY=value overrides; it is never sent over SSH.
type Endpoint struct {
	Host         string
	Path         string
	IdentityFile string
	Binary       string
	Port         int
	Env          []string
}

func (e Endpoint) IsLocal() bool { return e.Host == "" }

// Validate rejects ambiguous paths and SSH option injection before execution.
// Absolute binary paths may contain spaces or quotes: SSH shell escaping
// handles those characters without requiring a particular install directory.
func (e Endpoint) Validate() error {
	if e.Port < 0 || e.Port > 65535 {
		return errors.New("peer SSH port must be between 1 and 65535 (or 0 for the SSH default)")
	}
	if !e.IsLocal() {
		if config.ValidateHost(e.Host) != nil {
			return errors.New("peer SSH host must be a hostname, SSH alias, or user@host without whitespace or shell characters")
		}
		if len(e.Env) != 0 {
			return errors.New("environment overrides are supported only for local peers")
		}
	}
	for _, field := range []struct{ name, value string }{{"project root", e.Path}, {"SSH identity file", e.IdentityFile}} {
		if field.value != "" && (!filepath.IsAbs(field.value) || invalidPath(field.value)) {
			return fmt.Errorf("peer %s must be an absolute path without control characters", field.name)
		}
	}
	if e.Binary != "" && ((!filepath.IsAbs(e.Binary) && !validBinaryName(e.Binary)) || invalidPath(e.Binary)) {
		return errors.New("peer binary must be a command name or an absolute path without control characters")
	}
	for _, entry := range e.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.IndexByte(entry, 0) >= 0 {
			return errors.New("peer environment overrides must have the form KEY=value without NUL bytes")
		}
	}
	return nil
}

// Runner is the protocol boundary used by the CLI. It transports opaque input
// and output bytes, so the same interface carries JSON and encrypted packages.
type Runner interface {
	Run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error
}

type Transport struct {
	Endpoint Endpoint
	// Stderr optionally receives peer diagnostics separately from stdout.
	// Errors include at most 8 KiB of diagnostics, never command arguments,
	// package contents, environment variables, or captured stdout.
	Stderr io.Writer
}

func New(endpoint Endpoint) (*Transport, error) {
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	return &Transport{Endpoint: endpoint}, nil
}

// Run makes an Endpoint usable directly as a Runner. Construct a Transport
// instead when peer diagnostics should also be forwarded to a stderr writer.
func (e Endpoint) Run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	transport, err := New(e)
	if err != nil {
		return err
	}
	return transport.Run(ctx, args, stdin, stdout)
}

// Run starts a single peer command without a local shell. SSH's remote shell
// receives individually quoted arguments, including the executable. Host key
// verification is required, and authentication cannot prompt or consume the
// encrypted package from stdin.
func (t *Transport) Run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if ctx == nil {
		return errors.New("peer command requires a context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("peer command canceled: %w", err)
	}
	name, commandArgs, err := t.command(args)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, commandArgs...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.WaitDelay = 5 * time.Second
	if t.Endpoint.IsLocal() && len(t.Endpoint.Env) != 0 {
		cmd.Env = append(os.Environ(), t.Endpoint.Env...)
	}
	diagnostics := &limitedBuffer{limit: 8 << 10}
	cmd.Stderr = diagnostics
	if t.Stderr != nil {
		cmd.Stderr = io.MultiWriter(diagnostics, t.Stderr)
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("peer command canceled: %w", ctx.Err())
		}
		var remote otgerror.Error
		if json.Unmarshal([]byte(diagnostics.String()), &remote) == nil && remote.Code != "" && remote.Message != "" {
			return &remote
		}
		failure := &CommandError{Cause: err, Diagnostics: strings.TrimSpace(diagnostics.String())}
		if classified := t.classify(failure); classified != nil {
			return classified
		}
		return failure
	}
	return nil
}

// CommandError retains the subprocess error for errors.As(*exec.ExitError).
// Diagnostics contain stderr only and are bounded to avoid buffering arbitrary
// amounts of data from a failing or misconfigured peer.
type CommandError struct {
	Cause       error
	Diagnostics string
}

func (e *CommandError) Error() string {
	if e.Diagnostics != "" {
		return fmt.Sprintf("peer command failed: %v: %s", e.Cause, e.Diagnostics)
	}
	return fmt.Sprintf("peer command failed: %v", e.Cause)
}

func (e *CommandError) Unwrap() error { return e.Cause }

func (t *Transport) command(args []string) (string, []string, error) {
	if err := t.Endpoint.Validate(); err != nil {
		return "", nil, err
	}
	for _, arg := range args {
		if strings.IndexByte(arg, 0) >= 0 {
			return "", nil, errors.New("peer command arguments cannot contain NUL bytes")
		}
	}
	binary := t.Endpoint.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if t.Endpoint.IsLocal() {
		return binary, append([]string(nil), args...), nil
	}
	sshArgs := []string{"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=15"}
	if t.Endpoint.Port != 0 {
		sshArgs = append(sshArgs, "-p", strconv.Itoa(t.Endpoint.Port))
	}
	if t.Endpoint.IdentityFile != "" {
		sshArgs = append(sshArgs, "-i", t.Endpoint.IdentityFile)
	}
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, shellQuote(binary))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	sshArgs = append(sshArgs, "--", t.Endpoint.Host, strings.Join(quoted, " "))
	return "ssh", sshArgs, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func validBinaryName(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !asciiAlphaNumeric(c) && !strings.ContainsRune("_.-", c) {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func invalidPath(value string) bool {
	for _, c := range value {
		if unicode.IsControl(c) {
			return true
		}
	}
	return false
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if available := b.limit - b.Len(); available > 0 {
		if len(p) > available {
			p = p[:available]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

// DiscoverBinary asks the remote login shell for its installation path, so
// ~/.local/bin installations work even when ssh has a minimal nonlogin PATH.
func DiscoverBinary(ctx context.Context, endpoint Endpoint) (string, error) {
	if endpoint.IsLocal() {
		return exec.LookPath(DefaultBinary)
	}
	endpoint.Binary = "/bin/sh"
	endpoint.Path = ""
	var out bytes.Buffer
	if err := endpoint.Run(ctx, []string{"-c", `exec "${SHELL:-/bin/sh}" -lc 'command -v onthego'`}, nil, &out); err != nil {
		return "", fmt.Errorf("find onthego in remote login shell (or use --binary): %w", err)
	}
	found := strings.TrimSpace(out.String())
	if !filepath.IsAbs(found) || invalidPath(found) {
		return "", errors.New("remote login shell did not return an absolute onthego path; use --binary")
	}
	return found, nil
}

// ErrUnknownHostKey marks a first connection whose host key the user has not
// confirmed yet. Interactive pairing can offer to confirm it.
var ErrUnknownHostKey = errors.New("unknown SSH host key")

// ConfirmHostKey runs one interactive ssh connection on the user's terminal so
// OpenSSH can show the host key fingerprint and ask for confirmation.
func ConfirmHostKey(ctx context.Context, endpoint Endpoint, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	args := []string{"-o", "StrictHostKeyChecking=ask", "-o", "ConnectTimeout=15"}
	if endpoint.Port != 0 {
		args = append(args, "-p", strconv.Itoa(endpoint.Port))
	}
	if endpoint.IdentityFile != "" {
		args = append(args, "-i", endpoint.IdentityFile)
	}
	args = append(args, "--", endpoint.Host, "true")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// classify turns common SSH and installation failures into stable public
// errors that say what to do next. The original diagnostics stay as the cause.
func (t *Transport) classify(failure *CommandError) error {
	host := t.Endpoint.Host
	text := strings.ToLower(failure.Diagnostics)
	var exit *exec.ExitError
	code := -1
	if errors.As(failure.Cause, &exit) {
		code = exit.ExitCode()
	}
	if t.Endpoint.IsLocal() {
		if errors.Is(failure.Cause, exec.ErrNotFound) || errors.Is(failure.Cause, os.ErrNotExist) {
			return otgerror.Wrap(otgerror.CodeTargetUnavailable, "the onthego executable for this local device was not found; pair it again with --binary /absolute/path", failure)
		}
		return nil
	}
	switch {
	case code == 127 || strings.Contains(text, "command not found") || strings.Contains(text, "no such file or directory") && strings.Contains(text, "onthego"):
		return otgerror.Wrap(otgerror.CodeTargetUnavailable, fmt.Sprintf("onthego is not installed on %s or not on its login PATH; run onthego device install <name>, or pair again with --install", host), failure)
	case code != 255:
		return nil
	case strings.Contains(text, "remote host identification has changed"):
		return otgerror.Wrap(otgerror.CodeTargetAuth, fmt.Sprintf("the SSH host key for %s changed; verify the machine, then update ~/.ssh/known_hosts", host), failure)
	case strings.Contains(text, "host key verification failed") || strings.Contains(text, "no matching host key") || strings.Contains(text, "host key is known") || strings.Contains(text, "authenticity of host"):
		return otgerror.Wrap(otgerror.CodeTargetAuth, fmt.Sprintf("%s is not in known_hosts yet; connect once with ssh %s and confirm the host key", host, host), fmt.Errorf("%w: %w", ErrUnknownHostKey, failure))
	case strings.Contains(text, "permission denied") || strings.Contains(text, "too many authentication failures"):
		return otgerror.Wrap(otgerror.CodeTargetAuth, fmt.Sprintf("SSH login to %s needs a key that works without a password prompt; run ssh-copy-id %s, or load your key with ssh-add", host, host), failure)
	case strings.Contains(text, "could not resolve hostname") || strings.Contains(text, "name or service not known") || strings.Contains(text, "nodename nor servname"):
		return otgerror.Wrap(otgerror.CodeTargetUnavailable, fmt.Sprintf("cannot resolve %s; check the name in ~/.ssh/config, or run onthego tailscale if it is a tailnet device", host), failure)
	case strings.Contains(text, "connection refused"):
		return otgerror.Wrap(otgerror.CodeTargetUnavailable, fmt.Sprintf("%s refused SSH; enable its SSH server (macOS: System Settings > General > Sharing > Remote Login)", host), failure)
	case strings.Contains(text, "timed out") || strings.Contains(text, "no route to host") || strings.Contains(text, "network is unreachable") || strings.Contains(text, "connection closed") || strings.Contains(text, "connection reset"):
		return otgerror.Wrap(otgerror.CodeTargetUnavailable, fmt.Sprintf("%s is unreachable; make sure it is awake and on the same network or tailnet", host), failure)
	}
	return otgerror.Wrap(otgerror.CodeTargetUnavailable, fmt.Sprintf("SSH connection to %s failed; try ssh %s to see the full error", host, host), failure)
}
