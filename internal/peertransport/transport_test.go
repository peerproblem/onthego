package peertransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peerproblem/onthego/internal/otgerror"
)

func TestEndpointValidation(t *testing.T) {
	for _, endpoint := range []Endpoint{
		{},
		{Binary: "onthego", Env: []string{"ONTHEGO_HOME=/tmp/home"}},
		{Host: "local"},
		{Host: "alice@my-mac.local", Path: "/Users/alice/project with spaces", Port: 2222},
		{Host: "dev-box", Binary: "/opt/ON THE GO/it's-onthego", IdentityFile: "/Users/me/.ssh/id_ed25519"},
		{Host: "alice@[2001:db8::1]"},
	} {
		if err := endpoint.Validate(); err != nil {
			t.Errorf("valid endpoint %#v: %v", endpoint, err)
		}
	}
	for _, endpoint := range []Endpoint{
		{Host: "-oProxyCommand=evil"},
		{Host: "host;evil"},
		{Host: "host\n-oProxyCommand=evil"},
		{Host: "host -p22"},
		{Host: "ssh://host"},
		{Host: "$(evil)"},
		{Host: "user@user@host"},
		{Host: "host", Env: []string{"TOKEN=secret"}},
		{Path: "relative/project"},
		{Path: "/tmp/project\x00"},
		{IdentityFile: "~/.ssh/key"},
		{IdentityFile: "/tmp/key\n"},
		{Binary: "./onthego"},
		{Binary: "onthego;evil"},
		{Binary: "-onthego"},
		{Binary: ".."},
		{Binary: "/tmp/onthego\x00"},
		{Port: -1},
		{Port: 65536},
		{Env: []string{"INVALID"}},
		{Env: []string{"=value"}},
		{Env: []string{"KEY=\x00"}},
	} {
		if err := endpoint.Validate(); err == nil {
			t.Errorf("invalid endpoint accepted: %#v", endpoint)
		}
	}
}

func TestSSHCommandRequiresKnownHostAndQuotesArguments(t *testing.T) {
	transport := &Transport{Endpoint: Endpoint{Host: "user@machine", Port: 2222, IdentityFile: "/tmp/key with spaces"}}
	name, args, err := transport.command([]string{"peer", "export", "--root", "/tmp/it's $(echo unsafe); a project"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=15",
		"-p", "2222", "-i", "/tmp/key with spaces", "--", "user@machine",
		"'onthego' 'peer' 'export' '--root' '/tmp/it'\"'\"'s $(echo unsafe); a project'",
	}
	if name != "ssh" || !reflect.DeepEqual(args, want) {
		t.Fatalf("command = %q %#v, want ssh %#v", name, args, want)
	}
	if _, _, err := transport.command([]string{"arg\x00"}); err == nil {
		t.Fatal("NUL argument accepted")
	}
}

func TestLocalIsValidSSHHostname(t *testing.T) {
	transport := &Transport{Endpoint: Endpoint{Host: "local"}}
	name, _, err := transport.command([]string{"peer", "identity"})
	if err != nil || name != "ssh" {
		t.Fatalf("SSH alias local must connect over SSH, got %q: %v", name, err)
	}
}

func TestSSHRemoteShellPreservesArguments(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	executable := helperExecutable(t)
	link := filepath.Join(t.TempDir(), "onthego with 'quotes'")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	transport := &Transport{Endpoint: Endpoint{Host: "machine", Binary: link}}
	want := []string{"", "two words", "'quoted'", "line\nbreak", "$(exit 99)", "`exit 99`", "; exit 99", "--flag", "$HOME", "*"}
	_, args, err := transport.command(append(helperArgs("echo"), want...))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), shell, "-c", args[len(args)-1])
	cmd.Env = append(os.Environ(), "ONTHEGO_TEST_PEER_HELPER=1")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("remote shell: %v", err)
	}
	var got []string
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("invalid response %q: %v", output, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remote argv = %#v, want %#v", got, want)
	}
}

func TestLocalTransportStreamsBytesAndOverridesHome(t *testing.T) {
	transport, err := New(Endpoint{Binary: helperExecutable(t), Env: []string{"ONTHEGO_TEST_PEER_HELPER=1", "ONTHEGO_HOME=/tmp/isolated peer"}})
	if err != nil {
		t.Fatal(err)
	}
	input := bytes.Repeat([]byte{0, 1, 2, 3, 0xff, '\n'}, 10000)
	var output bytes.Buffer
	if err := transport.Run(context.Background(), helperArgs("copy"), bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), input) {
		t.Fatal("binary transfer changed the input")
	}
	output.Reset()
	if err := transport.Run(context.Background(), helperArgs("home"), nil, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "/tmp/isolated peer" {
		t.Fatalf("home override = %q", output.String())
	}
}

func TestFailureKeepsDiagnosticsSeparateAndBounded(t *testing.T) {
	var output, diagnostics bytes.Buffer
	transport := &Transport{
		Endpoint: Endpoint{Binary: helperExecutable(t), Env: []string{"ONTHEGO_TEST_PEER_HELPER=1", "ONTHEGO_TEST_SECRET=environment-secret"}},
		Stderr:   &diagnostics,
	}
	err := transport.Run(context.Background(), append(helperArgs("fail"), "argument-secret"), strings.NewReader("input-secret"), &output)
	var failure *CommandError
	var exit *exec.ExitError
	if !errors.As(err, &failure) || !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("error = %v; want exit status 7", err)
	}
	if output.String() != "output-not-a-diagnostic" {
		t.Fatalf("stdout was mixed with diagnostics: %q", output.String())
	}
	if len(failure.Diagnostics) > 8<<10 || !strings.HasPrefix(failure.Diagnostics, "peer diagnostic:") {
		t.Fatalf("invalid bounded diagnostic: length %d", len(failure.Diagnostics))
	}
	if diagnostics.Len() <= len(failure.Diagnostics) {
		t.Fatal("explicit stderr writer should receive the entire diagnostic")
	}
	for _, secret := range []string{"output-not-a-diagnostic", "argument-secret", "input-secret", "environment-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error exposed %q", secret)
		}
	}
}

func TestCanceledTransport(t *testing.T) {
	transport := &Transport{Endpoint: Endpoint{Binary: helperExecutable(t), Env: []string{"ONTHEGO_TEST_PEER_HELPER=1"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := transport.Run(ctx, helperArgs("wait"), nil, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if err := transport.Run(nil, nil, nil, nil); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestMissingLocalBinary(t *testing.T) {
	transport := &Transport{Endpoint: Endpoint{Binary: filepath.Join(t.TempDir(), "missing-onthego")}}
	var output bytes.Buffer
	if err := transport.Run(context.Background(), []string{"peer", "identity"}, nil, &output); err == nil {
		t.Fatal("missing binary succeeded")
	}
	if output.Len() != 0 {
		t.Fatal("failure contaminated the protocol output")
	}
}

func helperExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func helperArgs(mode string) []string {
	return []string{"-test.run=^TestPeerHelperProcess$", "--", mode}
}

func TestPeerHelperProcess(t *testing.T) {
	if os.Getenv("ONTHEGO_TEST_PEER_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	switch args[1] {
	case "echo":
		_ = json.NewEncoder(os.Stdout).Encode(args[2:])
	case "copy":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "home":
		_, _ = io.WriteString(os.Stdout, os.Getenv("ONTHEGO_HOME"))
	case "fail":
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = io.WriteString(os.Stdout, "output-not-a-diagnostic")
		_, _ = io.WriteString(os.Stderr, "peer diagnostic:"+strings.Repeat("x", 16<<10))
		os.Exit(7)
	case "wait":
		time.Sleep(30 * time.Second)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestStructuredRemoteErrorPreservesCodeAndRecoveryPath(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "peer")
	data := "#!/bin/sh\nprintf '%s\\n' '{\"code\":\"E_GENERATION_CONFLICT\",\"message\":\"destination changed\",\"preserved_path\":\"refs/onthego/backup/123\"}' >&2\nexit 6\n"
	if err := os.WriteFile(binary, []byte(data), 0o700); err != nil {
		t.Fatal(err)
	}
	err := (Endpoint{Binary: binary}).Run(context.Background(), nil, nil, io.Discard)
	var remote *otgerror.Error
	if !errors.As(err, &remote) || remote.Code != otgerror.CodeGenerationConflict || remote.Preserved != "refs/onthego/backup/123" {
		t.Fatalf("remote error = %#v", err)
	}
}

func TestDiscoverBinaryUsesRemoteLoginShell(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "login-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\n[ \"$1\" = -lc ] || exit 7\n[ \"$2\" = 'command -v onthego' ] || exit 8\nprintf '%s\\n' '/home/user/.local/bin/onthego'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeSSH := filepath.Join(dir, "ssh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\nfor arg do command=$arg; done\nexec /bin/sh -c \"$command\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	found, err := DiscoverBinary(context.Background(), Endpoint{Host: "paired-machine"})
	if err != nil || found != "/home/user/.local/bin/onthego" {
		t.Fatalf("discovery = %q, %v", found, err)
	}
}
