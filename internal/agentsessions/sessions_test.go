package agentsessions

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeRoundTripPreservesHistoryAndRemapsPaths(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "mini", "project.with_space")
	target := filepath.Join(base, "laptop", "project")
	roots := Roots{filepath.Join(base, "source-codex"), filepath.Join(base, "source-claude")}
	destination := Roots{filepath.Join(base, "target-codex"), filepath.Join(base, "target-claude")}
	codexPath := "sessions/2026/10/16/rollout-native.jsonl"
	codex := codexLog("native-session", source) + jsonLine(map[string]any{
		"type": "turn_context", "payload": map[string]any{
			"cwd": filepath.Join(source, "subdir"), "model": "gpt-6", "effort": "xhigh", "service_tier": "fast",
			"sandbox_policy": map[string]any{"writable_roots": []string{source, source + "-other"}},
		},
	}) + `{"type":"response_item","payload":{"encrypted_content":"opaque-original","text":"` + source + `/message stays unchanged","large_integer":9007199254740993}}` + "\n"
	writeTest(t, filepath.Join(roots.CodexHome, codexPath), codex)
	writeTest(t, filepath.Join(roots.CodexHome, "archived_sessions", "rollout-archived.jsonl"), codexLog("archived", source))
	writeTest(t, filepath.Join(roots.CodexHome, "sessions", "foreign.jsonl"), codexLog("foreign", source+"-other"))
	writeTest(t, filepath.Join(roots.CodexHome, "auth.json"), `{"access_token":"do not copy"}`)
	writeTest(t, filepath.Join(roots.CodexHome, "state_5.sqlite"), "do not copy")
	claudePath := filepath.Join("projects", ProjectDirectory(source), "claude-main.jsonl")
	claudeSub := filepath.Join("projects", ProjectDirectory(source), "claude-main", "subagents", "agent-fork.jsonl")
	writeTest(t, filepath.Join(roots.ClaudeHome, claudePath), claudeLog("claude-main", source))
	writeTest(t, filepath.Join(roots.ClaudeHome, claudeSub), claudeLog("claude-main", source))
	writeTest(t, filepath.Join(roots.ClaudeHome, "projects", ProjectDirectory(source), "unrelated.jsonl"), claudeLog("foreign", source+"-other"))
	writeTest(t, filepath.Join(roots.ClaudeHome, ".credentials.json"), "do not copy")

	bundle := filepath.Join(base, "export")
	manifest, err := Export(source, bundle, roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Sessions) != 4 {
		t.Fatalf("sessions = %#v", manifest.Sessions)
	}
	if manifest.Sessions[0].Agent != "claude" {
		t.Fatal("manifest is not sorted deterministically")
	}
	for _, entry := range manifest.Sessions {
		if strings.Contains(entry.Path, "foreign") || strings.Contains(entry.Path, "auth") || strings.Contains(entry.Path, "credentials") {
			t.Fatalf("unexpected session %s", entry.Path)
		}
	}
	if got := readTest(t, filepath.Join(bundle, "codex", codexPath)); got != codex {
		t.Fatal("export modified native history")
	}
	unrelated := filepath.Join(destination.CodexHome, "sessions", "unrelated.jsonl")
	writeTest(t, unrelated, codexLog("unrelated", "/different-project"))
	preview, err := ValidateImport(bundle, target, destination)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Imported != 4 {
		t.Fatalf("preview = %#v", preview)
	}
	if _, err := os.Stat(filepath.Join(destination.CodexHome, codexPath)); !os.IsNotExist(err) {
		t.Fatal("preflight wrote a history")
	}
	result, err := Import(bundle, target, destination)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 4 || result.Skipped != 0 || result.Updated != 0 {
		t.Fatalf("result = %#v", result)
	}
	imported := readTest(t, filepath.Join(destination.CodexHome, codexPath))
	for _, want := range []string{target, target + "/subdir", "gpt-6", "xhigh", "fast", "opaque-original", "9007199254740993", source + "/message stays unchanged", source + "-other"} {
		if !strings.Contains(imported, want) {
			t.Errorf("import missing %q: %s", want, imported)
		}
	}
	if strings.Contains(imported, `"cwd":"`+source) {
		t.Fatal("cwd was not remapped")
	}
	if !strings.Contains(readTest(t, unrelated), "unrelated") {
		t.Fatal("unrelated session was replaced")
	}
	newClaudePath := filepath.Join(destination.ClaudeHome, "projects", ProjectDirectory(target), "claude-main", "subagents", "agent-fork.jsonl")
	if !strings.Contains(readTest(t, newClaudePath), target) {
		t.Fatal("Claude subagent was not remapped")
	}
	info, err := os.Stat(filepath.Join(destination.CodexHome, codexPath))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("native session mode = %v, err = %v", info.Mode(), err)
	}
	result, err = Import(bundle, target, destination)
	if err != nil || result.Skipped != 4 {
		t.Fatalf("repeat import = %#v, %v", result, err)
	}
}

func TestAppendOnlyUpdateAndConflictPreflight(t *testing.T) {
	base := t.TempDir()
	source, target := filepath.Join(base, "source-project"), filepath.Join(base, "target-project")
	roots := Roots{CodexHome: filepath.Join(base, "source-codex")}
	destination := Roots{CodexHome: filepath.Join(base, "target-codex")}
	path := filepath.Join("sessions", "rollout-a.jsonl")
	initial := codexLog("a", source)
	writeTest(t, filepath.Join(roots.CodexHome, path), initial)
	first := filepath.Join(base, "first")
	if _, err := Export(source, first, roots); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(first, target, destination); err != nil {
		t.Fatal(err)
	}
	newer := initial + `{"type":"event_msg","payload":{"message":"second turn"}}` + "\n"
	writeTest(t, filepath.Join(roots.CodexHome, path), newer)
	second := filepath.Join(base, "second")
	if _, err := Export(source, second, roots); err != nil {
		t.Fatal(err)
	}
	result, err := Import(second, target, destination)
	if err != nil || result.Updated != 1 {
		t.Fatalf("append import = %#v, %v", result, err)
	}
	if !strings.Contains(readTest(t, filepath.Join(destination.CodexHome, path)), "second turn") {
		t.Fatal("new turn missing")
	}
	if _, err := Import(first, target, destination); err == nil || !strings.Contains(err.Error(), "newer or diverged") {
		t.Fatalf("older import must conflict, got %v", err)
	}

	// Put a would-be new file first in lexical order, then a divergent log.
	writeTest(t, filepath.Join(roots.CodexHome, "sessions", "000-new.jsonl"), codexLog("new", source))
	writeTest(t, filepath.Join(roots.CodexHome, path), initial+`{"type":"event_msg","payload":{"message":"divergence"}}`+"\n")
	third := filepath.Join(base, "third")
	if _, err := Export(source, third, roots); err != nil {
		t.Fatal(err)
	}
	before := readTest(t, filepath.Join(destination.CodexHome, path))
	if _, err := Import(third, target, destination); err == nil {
		t.Fatal("divergent history was overwritten")
	}
	if _, err := os.Stat(filepath.Join(destination.CodexHome, "sessions", "000-new.jsonl")); !os.IsNotExist(err) {
		t.Fatal("conflict allowed partial import")
	}
	if readTest(t, filepath.Join(destination.CodexHome, path)) != before {
		t.Fatal("conflict changed existing history")
	}
}

func TestImportRejectsTamperingAndUnsafePaths(t *testing.T) {
	for _, scenario := range []string{"hash", "escape", "symlink", "unrelated", "duplicate", "unknown-agent", "credential"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			source, target := filepath.Join(base, "source"), filepath.Join(base, "target")
			roots := Roots{CodexHome: filepath.Join(base, "codex")}
			dest := Roots{CodexHome: filepath.Join(base, "target-codex")}
			writeTest(t, filepath.Join(roots.CodexHome, "sessions", "a.jsonl"), codexLog("a", source))
			bundle := filepath.Join(base, "bundle")
			manifest, err := Export(source, bundle, roots)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "hash":
				writeTest(t, filepath.Join(bundle, "codex", "sessions", "a.jsonl"), codexLog("changed", source))
			case "escape":
				manifest.Sessions[0].Path = "sessions/../../outside.jsonl"
			case "symlink":
				if err := os.MkdirAll(dest.CodexHome, 0o700); err != nil {
					t.Fatal(err)
				}
				outside := filepath.Join(base, "outside")
				if err := os.Mkdir(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dest.CodexHome, "sessions")); err != nil {
					t.Fatal(err)
				}
			case "unrelated":
				manifest.Sessions[0].CWD = "/somewhere-else"
			case "duplicate":
				manifest.Sessions = append(manifest.Sessions, manifest.Sessions[0])
			case "unknown-agent":
				manifest.Sessions[0].Agent = "anything"
			case "credential":
				manifest.Sessions[0].Path = "auth.json"
			}
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			writeTest(t, filepath.Join(bundle, "manifest.json"), string(encoded))
			if _, err := ValidateImport(bundle, target, dest); err == nil {
				t.Fatal("unsafe import passed preflight")
			}
		})
	}
}

func TestDiscoveryRejectsIncompleteProjectLogAndSkipsSymlinks(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	roots := Roots{CodexHome: filepath.Join(base, "codex")}
	path := filepath.Join(roots.CodexHome, "sessions", "valid.jsonl")
	writeTest(t, path, codexLog("valid", project))
	outside := filepath.Join(base, "outside.jsonl")
	writeTest(t, outside, codexLog("symlink", project))
	if err := os.Symlink(outside, filepath.Join(roots.CodexHome, "sessions", "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	sessions, err := Discover(project, roots)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("discovered = %#v, %v", sessions, err)
	}
	writeTest(t, path, codexLog("valid", project)+`{"partial":`)
	if _, err := Discover(project, roots); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("incomplete active log accepted: %v", err)
	}
}

func TestUnrelatedOversizedLogDoesNotBlockProjectDiscovery(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	roots := Roots{CodexHome: filepath.Join(base, "codex")}
	writeTest(t, filepath.Join(roots.CodexHome, "sessions", "valid.jsonl"), codexLog("valid", project))
	other := filepath.Join(roots.CodexHome, "sessions", "huge-unrelated.jsonl")
	writeTest(t, other, codexLog("unrelated", project+"-other"))
	if err := os.Truncate(other, MaxSessionBytes+1); err != nil {
		t.Fatal(err)
	}
	sessions, err := Discover(project, roots)
	if err != nil || len(sessions) != 1 || sessions[0].ID != "valid" {
		t.Fatalf("discovery = %#v, %v", sessions, err)
	}
}

func TestOSAncestorSymlinkIsAllowedButAgentHomeSymlinkIsRejected(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(base, "project")
	home := filepath.Join(alias, "codex")
	writeTest(t, filepath.Join(home, "sessions", "valid.jsonl"), codexLog("valid", project))
	if _, err := Discover(project, Roots{CodexHome: home}); err != nil {
		t.Fatalf("OS-style ancestor symlink rejected: %v", err)
	}
	if _, err := Discover(project, Roots{CodexHome: alias}); err == nil {
		t.Fatal("symlinked agent home accepted")
	}
}

func TestRollbackPreservesExistingBytesAndRemovesNewFiles(t *testing.T) {
	base := t.TempDir()
	previous := []byte("original history\n")
	incoming := []byte("imported history\n")
	plans := []writePlan{{home: base, relative: "sessions/a.jsonl", previous: previous, incoming: incoming, exists: true}, {home: base, relative: "sessions/b.jsonl", incoming: incoming}}
	for _, plan := range plans {
		writeTest(t, filepath.Join(base, plan.relative), string(incoming))
	}
	if err := rollback(plans); err != nil {
		t.Fatal(err)
	}
	if got := readTest(t, filepath.Join(base, "sessions/a.jsonl")); got != string(previous) {
		t.Fatalf("rolled back bytes = %q", got)
	}
	if _, err := os.Stat(filepath.Join(base, "sessions/b.jsonl")); !os.IsNotExist(err) {
		t.Fatal("new history remained after rollback")
	}
	writeTest(t, filepath.Join(base, "sessions/a.jsonl"), "concurrently changed")
	if err := rollback(plans[:1]); err == nil {
		t.Fatal("rollback overwrote an external change")
	}
	if got := readTest(t, filepath.Join(base, "sessions/a.jsonl")); got != "concurrently changed" {
		t.Fatal("external change lost")
	}
}

func TestDefaultRootsUseAgentOverrides(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	roots, err := DefaultRoots()
	if err != nil || roots.CodexHome != "/custom/codex" || roots.ClaudeHome != "/custom/claude" {
		t.Fatalf("roots = %#v, %v", roots, err)
	}
}

func TestCanonicalRewriteRetainsOpaqueFieldsAndNumericPrecision(t *testing.T) {
	data := []byte(`{"text":"/old/project untouched","cwd":"/old/project","opaque":"abc123","value":9007199254740993,"path":"/old/project-other/file"}` + "\n")
	result, err := rewrite(data, "/old/project", "/new/project")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"text":"/old/project untouched"`, `"cwd":"/new/project"`, `"opaque":"abc123"`, `9007199254740993`, `/old/project-other/file`} {
		if !bytes.Contains(result, []byte(want)) {
			t.Fatalf("rewrite lost %s: %s", want, result)
		}
	}
}

func jsonLine(value any) string { encoded, _ := json.Marshal(value); return string(encoded) + "\n" }

func codexLog(id, cwd string) string {
	return jsonLine(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": cwd, "forked_from_id": "parent-id"}})
}

func claudeLog(id, cwd string) string {
	return jsonLine(map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "message": map[string]any{"role": "user", "content": "continue"}})
}

func writeTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
