package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jbaehova/onthego/internal/agentsessions"
)

// Exercise the installed executable and private subprocess protocol with two
// independent device homes. No real agent login, remote service, or API key is
// involved, and no test changes the Go process's working directory/environment.
func TestCLIIntegration(t *testing.T) {
	binary := buildCLI(t)
	t.Run("TwoDeviceHandoff", func(t *testing.T) {
		base := t.TempDir()
		a := newMachine(t, base, "desktop", binary)
		b := newMachine(t, base, "laptop", binary)
		makeRepository(t, a)

		// Broken dotenv contents and permissive permissions must not affect CLI
		// startup; these are application data, never ONTHEGO configuration.
		dotenv := "not valid dotenv [\nOPENAI_API_KEY=\"unterminated\n"
		writeFixture(t, filepath.Join(a.root, ".env"), dotenv)
		if err := os.Chmod(filepath.Join(a.root, ".env"), 0o666); err != nil {
			t.Fatal(err)
		}
		a.mustRun(t, "version")
		a.mustRun(t, "init")
		a.mustRun(t, "status")
		assertFile(t, filepath.Join(a.root, ".env"), dotenv)
		if info, err := os.Stat(filepath.Join(a.root, ".env")); err != nil || info.Mode().Perm() != 0o666 {
			t.Fatalf("startup changed dotenv permissions: %v, %v", info, err)
		}
		a.git(t, "add", ".onthego.yaml", ".gitignore")
		a.git(t, "commit", "-qm", "Initialize portable project configuration")
		a.git(t, "remote", "add", "origin", "https://github.com/example/handoff-fixture.git")

		writeFixture(t, filepath.Join(a.root, "tracked.txt"), "staged change\n")
		a.git(t, "add", "tracked.txt")
		writeFixture(t, filepath.Join(a.root, "tracked.txt"), "staged and unstaged changes\n")
		writeFixture(t, filepath.Join(a.root, "untracked.bin"), string([]byte{0, 1, 2, 0xff, '\n'}))
		if err := os.Remove(filepath.Join(a.root, "removed.txt")); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(a.root, "ignored-data", "notes.txt"), "explicitly selected ignored data\n")
		writeFixture(t, filepath.Join(a.root, "cache", "machine-local.txt"), "desktop cache\n")
		codexRelative := filepath.Join("sessions", "2026", "10", "16", "rollout-native.jsonl")
		codexOriginal := jsonLine(t, map[string]any{"type": "session_meta", "payload": map[string]any{
			"id": "0199a88c-1111-7222-8333-444455556666", "cwd": a.root, "forked_from_id": "parent-session",
		}}) + jsonLine(t, map[string]any{"type": "turn_context", "payload": map[string]any{
			"cwd": a.root, "model": "gpt-6", "effort": "xhigh", "service_tier": "fast",
		}}) + jsonLine(t, map[string]any{"type": "response_item", "payload": map[string]any{
			"message": "keep the original discussion of " + a.root, "encrypted_content": "opaque-session-content",
		}})
		writeFixture(t, filepath.Join(a.codex, codexRelative), codexOriginal)
		writeFixture(t, filepath.Join(a.codex, "sessions", "unrelated.jsonl"), jsonLine(t, map[string]any{
			"type": "session_meta", "payload": map[string]any{"id": "unrelated", "cwd": a.root + "-different-project"},
		}))
		writeFixture(t, filepath.Join(a.codex, "auth.json"), `{"access_token":"machine-a-login-must-not-transfer"}`)
		writeFixture(t, filepath.Join(a.codex, "state_5.sqlite"), "machine-a-sidebar-must-not-transfer")
		claudeRelative := filepath.Join("projects", agentsessions.ProjectDirectory(a.root), "claude-main.jsonl")
		writeFixture(t, filepath.Join(a.claude, claudeRelative), jsonLine(t, map[string]any{
			"type": "user", "sessionId": "claude-main", "cwd": a.root, "parentUuid": "prior-turn", "message": map[string]any{"role": "user", "content": "continue here"},
		}))
		writeFixture(t, filepath.Join(a.claude, ".credentials.json"), `{"token":"claude-login-must-not-transfer"}`)

		wrapper := b.wrapper(t)
		a.mustRun(t, "device", "add", "laptop", "--local-path", b.root, "--binary", wrapper)
		a.mustRun(t, "doctor", "laptop")
		assertPublicIdentity(t, a.mustRun(t, "--json", "identity"))
		assertPublicIdentity(t, a.mustRun(t, "peer", "identity"))
		var push struct {
			Root     string `json:"root"`
			Manifest struct {
				SnapshotID string `json:"snapshot_id"`
			} `json:"manifest"`
		}
		decodeJSON(t, a.mustRun(t, "--json", "push", "laptop", "--include", "ignored-data"), &push)
		if push.Root != b.root || push.Manifest.SnapshotID == "" {
			t.Fatalf("invalid push receipt: %+v", push)
		}
		assertSameGitState(t, a, b)
		assertFile(t, filepath.Join(b.root, "ignored-data", "notes.txt"), "explicitly selected ignored data\n")
		assertFile(t, filepath.Join(b.root, "untracked.bin"), string([]byte{0, 1, 2, 0xff, '\n'}))
		for _, path := range []string{
			filepath.Join(b.root, ".env"), filepath.Join(b.root, "cache", "machine-local.txt"), filepath.Join(b.root, "removed.txt"),
			filepath.Join(b.codex, "auth.json"), filepath.Join(b.codex, "state_5.sqlite"), filepath.Join(b.codex, "sessions", "unrelated.jsonl"), filepath.Join(b.claude, ".credentials.json"),
		} {
			assertAbsent(t, path)
		}
		assertFile(t, filepath.Join(a.codex, codexRelative), codexOriginal)
		assertCodexMetadata(t, filepath.Join(b.codex, codexRelative), b.root, a.root)
		claudeB := filepath.Join(b.claude, "projects", agentsessions.ProjectDirectory(b.root), "claude-main.jsonl")
		assertJSONLContains(t, claudeB, "cwd", b.root)
		assertUnchangedStatus(t, a.mustRun(t, "--json", "status"))
		assertUnchangedStatus(t, a.mustRun(t, "--json", "status", "laptop"))
		if got := strings.TrimSpace(b.git(t, "remote", "get-url", "origin")); got != "https://github.com/example/handoff-fixture.git" {
			t.Fatalf("portable Git origin missing: %q", got)
		}

		// Continue on the laptop, including its native histories, then receive
		// the changes on the desktop without changing the source Git index.
		writeFixture(t, filepath.Join(b.root, "tracked.txt"), "continued on laptop\n")
		writeFixture(t, filepath.Join(b.root, "from-laptop.txt"), "new laptop work\n")
		writeFixture(t, filepath.Join(b.root, "ignored-data", "notes.txt"), "continued ignored data\n")
		appendFixture(t, filepath.Join(b.codex, codexRelative), jsonLine(t, map[string]any{"type": "event_msg", "payload": map[string]any{"message": "laptop continuation"}}))
		appendFixture(t, claudeB, jsonLine(t, map[string]any{"type": "assistant", "sessionId": "claude-main", "cwd": b.root, "message": map[string]any{"role": "assistant", "content": "continued on laptop"}}))
		var pull struct {
			Root   string `json:"root"`
			Backup string `json:"backup"`
		}
		decodeJSON(t, a.mustRun(t, "--json", "pull", "laptop"), &pull)
		if pull.Root != a.root || pull.Backup == "" {
			t.Fatalf("pull did not retain the previous workspace: %+v", pull)
		}
		assertSameGitState(t, a, b)
		assertFile(t, filepath.Join(a.root, "tracked.txt"), "continued on laptop\n")
		assertFile(t, filepath.Join(a.root, "from-laptop.txt"), "new laptop work\n")
		assertFile(t, filepath.Join(a.root, "ignored-data", "notes.txt"), "continued ignored data\n")
		assertFile(t, filepath.Join(a.root, ".env"), dotenv)
		assertFile(t, filepath.Join(a.root, "cache", "machine-local.txt"), "desktop cache\n")
		assertFile(t, filepath.Join(pull.Backup, "tracked.txt"), "staged and unstaged changes\n")
		assertCodexMetadata(t, filepath.Join(a.codex, codexRelative), a.root, a.root)
		assertJSONLContains(t, filepath.Join(a.codex, codexRelative), "message", "laptop continuation")
		assertJSONLContains(t, filepath.Join(a.claude, claudeRelative), "cwd", a.root)
		assertUnchangedStatus(t, a.mustRun(t, "--json", "status"))
		assertUnchangedStatus(t, a.mustRun(t, "--json", "status", "laptop"))

		// Concurrent work is rejected instead of silently overwriting either
		// workspace or its histories.
		writeFixture(t, filepath.Join(a.root, "tracked.txt"), "desktop diverged\n")
		writeFixture(t, filepath.Join(b.root, "tracked.txt"), "laptop diverged\n")
		beforeSession := readFixture(t, filepath.Join(a.codex, codexRelative))
		stdout, stderr, err := a.run(t, "--json", "pull", "laptop")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 6 {
			t.Fatalf("conflicting pull = %v, stdout=%s, stderr=%s; want conflict exit 6", err, stdout, stderr)
		}
		if !strings.Contains(stderr, "local file or agent session changes") {
			t.Fatalf("conflict diagnostic missing: %s", stderr)
		}
		assertFile(t, filepath.Join(a.root, "tracked.txt"), "desktop diverged\n")
		assertFile(t, filepath.Join(b.root, "tracked.txt"), "laptop diverged\n")
		assertFile(t, filepath.Join(a.codex, codexRelative), beforeSession)

		// A rejected push must keep the sender dirty and retain its last
		// acknowledged snapshot instead of recording an undelivered export.
		var before, after struct {
			SnapshotID  string `json:"snapshot_id"`
			HasBaseline bool   `json:"has_baseline"`
			Changed     bool   `json:"changed"`
		}
		decodeJSON(t, a.mustRun(t, "--json", "status"), &before)
		stdout, stderr, err = a.run(t, "--json", "push", "laptop")
		if err == nil {
			t.Fatalf("conflicting push succeeded: stdout=%s stderr=%s", stdout, stderr)
		}
		decodeJSON(t, a.mustRun(t, "--json", "status"), &after)
		if !before.Changed || !after.Changed || !after.HasBaseline || after.SnapshotID != before.SnapshotID {
			t.Fatalf("failed push changed acknowledged state: before=%+v after=%+v", before, after)
		}
		assertFile(t, filepath.Join(a.root, "tracked.txt"), "desktop diverged\n")
		assertFile(t, filepath.Join(b.root, "tracked.txt"), "laptop diverged\n")
	})
	t.Run("PersistentSecretPreview", func(t *testing.T) {
		base := t.TempDir()
		a := newMachine(t, base, "desktop", binary)
		b := newMachine(t, base, "laptop", binary)
		makeRepository(t, a)
		a.mustRun(t, "init")
		first := "APPLICATION_SECRET=private-application-value-v1\n"
		second := "APPLICATION_SECRET=private-application-value-v2\n"
		writeFixture(t, filepath.Join(a.root, ".env"), first)
		a.mustRun(t, "device", "add", "laptop", "--local-path", b.root, "--binary", b.wrapper(t))
		a.mustRun(t, "push", "laptop", "--include-secret", ".env")
		assertFile(t, filepath.Join(b.root, ".env"), first)
		assertUnchangedStatus(t, a.mustRun(t, "--json", "status"))
		writeFixture(t, filepath.Join(a.root, ".env"), second)

		// The secret opt-in is now persistent transfer policy. Every preview
		// must describe the subsequent real export, even without another flag.
		for _, preview := range []struct {
			args  []string
			field string
		}{
			{[]string{"--json", "push", "laptop", "--dry-run"}, "selected_files"},
			{[]string{"--json", "snapshot", "pack", "--dry-run"}, "selected_files"},
			{[]string{"--json", "inspect"}, "files"},
		} {
			output := a.mustRun(t, preview.args...)
			assertSecretPreview(t, output, preview.field, ".env")
			if strings.Contains(output, "private-application-value") {
				t.Fatalf("preview printed secret contents: %s", output)
			}
		}
		assertFile(t, filepath.Join(b.root, ".env"), first)
		var status struct {
			Changed bool `json:"changed"`
		}
		decodeJSON(t, a.mustRun(t, "--json", "status"), &status)
		if !status.Changed {
			t.Fatal("preview acknowledged or overlooked an unsent managed secret change")
		}
		a.mustRun(t, "push", "laptop")
		assertFile(t, filepath.Join(b.root, ".env"), second)
		assertUnchangedStatus(t, a.mustRun(t, "--json", "status"))
	})
}

type testMachine struct {
	root, data, codex, claude, binary string
	env                               []string
}

func buildCLI(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find repository root")
	}
	binary := filepath.Join(t.TempDir(), "onthego")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "./cmd/onthego")
	cmd.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	return binary
}

func newMachine(t *testing.T, base, name, binary string) testMachine {
	t.Helper()
	// macOS temporary directories commonly use /var -> /private/var. Native
	// sessions and project roots must agree on the same canonical directory.
	var err error
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(base, name+" user")
	m := testMachine{root: filepath.Join(base, name+" project"), data: filepath.Join(user, "onthego"), codex: filepath.Join(user, "codex"), claude: filepath.Join(user, "claude"), binary: binary}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ONTHEGO_") || strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "DAYTONA_") || strings.HasPrefix(key, "GIT_") || key == "CODEX_HOME" || key == "CLAUDE_CONFIG_DIR" {
			continue
		}
		m.env = append(m.env, entry)
	}
	m.env = append(m.env, "ONTHEGO_HOME="+m.data, "CODEX_HOME="+m.codex, "CLAUDE_CONFIG_DIR="+m.claude, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	return m
}

func makeRepository(t *testing.T, m testMachine) {
	t.Helper()
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		t.Fatal(err)
	}
	m.git(t, "init", "-q", "-b", "main")
	m.git(t, "config", "user.name", "ONTHEGO Test")
	m.git(t, "config", "user.email", "onthego@example.invalid")
	writeFixture(t, filepath.Join(m.root, "tracked.txt"), "original\n")
	writeFixture(t, filepath.Join(m.root, "removed.txt"), "remove after commit\n")
	writeFixture(t, filepath.Join(m.root, ".gitignore"), "ignored-data/\ncache/\n")
	m.git(t, "add", ".")
	m.git(t, "commit", "-qm", "Initial commit")
}

func (m testMachine) wrapper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(m.root), "peer 'laptop' wrapper")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	data := "#!/bin/sh\nexport ONTHEGO_HOME=" + quote(m.data) + "\nexport CODEX_HOME=" + quote(m.codex) + "\nexport CLAUDE_CONFIG_DIR=" + quote(m.claude) + "\nexec " + quote(m.binary) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func (m testMachine) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.binary, args...)
	cmd.Dir, cmd.Env = m.root, m.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (m testMachine) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := m.run(t, args...)
	if err != nil {
		t.Fatalf("onthego %v: %v\nstdout: %s\nstderr: %s", args, err, stdout, stderr)
	}
	return stdout
}

func (m testMachine) git(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = m.root, m.env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, m.root, err, output)
	}
	return string(output)
}

func assertSameGitState(t *testing.T, a, b testMachine) {
	t.Helper()
	for _, args := range [][]string{{"rev-parse", "HEAD"}, {"symbolic-ref", "HEAD"}, {"diff", "--cached", "--binary"}, {"diff", "--binary"}, {"status", "--porcelain=v1", "-z", "--untracked-files=all"}} {
		left, right := a.git(t, args...), b.git(t, args...)
		if left != right {
			t.Fatalf("git %v differs between devices\nsource: %q\ntarget: %q", args, left, right)
		}
	}
}

func assertPublicIdentity(t *testing.T, data string) {
	t.Helper()
	var result map[string]any
	decodeJSON(t, data, &result)
	if len(result) != 3 || result["schema_version"] != float64(2) {
		t.Fatalf("identity exposed unexpected fields: %v", result)
	}
	for _, field := range []string{"age_recipient", "signing_public"} {
		if value, ok := result[field].(string); !ok || value == "" {
			t.Fatalf("identity has no %s", field)
		}
	}
}

func assertSecretPreview(t *testing.T, data, field, path string) {
	t.Helper()
	var response map[string]json.RawMessage
	decodeJSON(t, data, &response)
	var files []struct {
		RelativePath string `json:"relative_path"`
		Secret       bool   `json:"secret"`
	}
	decodeJSON(t, string(response[field]), &files)
	for _, file := range files {
		if file.RelativePath == path {
			if !file.Secret {
				t.Fatalf("preview hides secret classification for %s: %s", path, data)
			}
			return
		}
	}
	t.Fatalf("preview omits persistent secret %s: %s", path, data)
}

func assertUnchangedStatus(t *testing.T, data string) {
	t.Helper()
	var status struct {
		HasBaseline bool `json:"has_baseline"`
		Changed     bool `json:"changed"`
	}
	decodeJSON(t, data, &status)
	if !status.HasBaseline || status.Changed {
		t.Fatalf("expected acknowledged unchanged workspace: %s", data)
	}
}

func assertCodexMetadata(t *testing.T, file, cwd, originalRoot string) {
	t.Helper()
	for key, value := range map[string]string{"cwd": cwd, "model": "gpt-6", "effort": "xhigh", "service_tier": "fast", "forked_from_id": "parent-session", "encrypted_content": "opaque-session-content", "message": "keep the original discussion of " + originalRoot} {
		assertJSONLContains(t, file, key, value)
	}
}

func assertJSONLContains(t *testing.T, file, key, value string) {
	t.Helper()
	var contains func(any) bool
	contains = func(v any) bool {
		switch item := v.(type) {
		case map[string]any:
			if item[key] == value {
				return true
			}
			for _, child := range item {
				if contains(child) {
					return true
				}
			}
		case []any:
			for _, child := range item {
				if contains(child) {
					return true
				}
			}
		}
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(readFixture(t, file)), "\n") {
		var row any
		decodeJSON(t, line, &row)
		if contains(row) {
			return
		}
	}
	t.Fatalf("native history %s is missing %s=%q", file, key, value)
}

func decodeJSON(t *testing.T, data string, result any) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), result); err != nil {
		t.Fatalf("invalid JSON response %q: %v", data, err)
	}
}

func jsonLine(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func writeFixture(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFixture(t *testing.T, path, value string) {
	t.Helper()
	writeFixture(t, path, readFixture(t, path)+value)
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	if got := readFixture(t, path); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("file unexpectedly transferred: %s (%v)", path, err)
	}
}
