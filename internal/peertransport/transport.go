// Package peertransport runs ONTHEGO directly on a user-owned peer. SSH uses
// the user's existing SSH configuration, credentials, and known-hosts file;
// it does not need a central service or an API key.
package peertransport

import (
	"bytes"
	"context"
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
		if !validHost(e.Host) {
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
		return &CommandError{Cause: err, Diagnostics: strings.TrimSpace(diagnostics.String())}
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

func validHost(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.Count(value, "@") > 1 {
		return false
	}
	for _, c := range value {
		if !asciiAlphaNumeric(c) && !strings.ContainsRune("_.-@%:[]", c) {
			return false
		}
	}
	return true
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
