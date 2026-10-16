package agentsessions

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRewritePreservesExactBytesAndOpaqueStructuredContent(t *testing.T) {
	original := []byte(" \r\n" + ` { "z": 1e+03, "cwd" : "/old/project", "literal":"<>&\\/", "payload": {"writable_roots" : ["/old/project/sub", "/other"]}, "message":{"cwd":"/old/project", "content":[{"path":"/old/project/file"}]}, "input":{"path":"/old/project/input"} }` + "\r\n" + `{"text":"/old/project","cwd":"/old/project/sub"}`)
	expected := bytes.Replace(original, []byte(`"cwd" : "/old/project"`), []byte(`"cwd" : "/new/<project>&"`), 1)
	expected = bytes.Replace(expected, []byte(`"writable_roots" : ["/old/project/sub"`), []byte(`"writable_roots" : ["/new/<project>&/sub"`), 1)
	expected = bytes.Replace(expected, []byte(`"text":"/old/project","cwd":"/old/project/sub"`), []byte(`"text":"/old/project","cwd":"/new/<project>&/sub"`), 1)
	actual, err := rewrite(original, "/old/project", "/new/<project>&")
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("rewrite changed bytes beyond path tokens:\nwant %q\ngot  %q\nerr %v", expected, actual, err)
	}
	unchanged, err := rewrite(original, "/old/project", "/old/project")
	if err != nil || !bytes.Equal(unchanged, original) {
		t.Fatal("identity rewrite modified bytes")
	}
}

func TestExportTrimsOnlyIncompleteTailAndSkipsMalformedSession(t *testing.T) {
	base := canonicalTestDir(t)
	project := filepath.Join(base, "project")
	roots := Roots{CodexHome: filepath.Join(base, "codex")}
	prefix := codexLog("truncated", project) + "\r\n" + `{"type":"event_msg","payload":{"message":"<intact>"}}` + "\r\n"
	writeTest(t, filepath.Join(roots.CodexHome, "sessions", "truncated.jsonl"), prefix+`{"type":"event_msg","pay`)
	writeTest(t, filepath.Join(roots.CodexHome, "sessions", "broken.jsonl"), codexLog("broken", project)+"{malformed}\n"+`{"valid":true}`+"\n")
	noNewline := strings.TrimSuffix(codexLog("no-newline", project), "\n")
	writeTest(t, filepath.Join(roots.CodexHome, "sessions", "no-newline.jsonl"), noNewline)
	bundle := filepath.Join(base, "bundle")
	manifest, err := Export(project, bundle, roots)
	if err != nil || len(manifest.Sessions) != 2 {
		t.Fatalf("export: %#v %v", manifest, err)
	}
	if len(manifest.Warnings) != len(warnings)+2 {
		t.Fatalf("missing explicit corruption warnings: %#v", manifest.Warnings)
	}
	if got := readTest(t, filepath.Join(bundle, "codex", "sessions", "truncated.jsonl")); got != prefix {
		t.Fatalf("trimmed content changed: %q", got)
	}
	if got := readTest(t, filepath.Join(bundle, "codex", "sessions", "no-newline.jsonl")); got != noNewline {
		t.Fatal("valid final line was modified")
	}
	if _, err := os.Stat(filepath.Join(bundle, "codex", "sessions", "broken.jsonl")); !os.IsNotExist(err) {
		t.Fatal("malformed session exported")
	}
	if got := readTest(t, filepath.Join(roots.CodexHome, "sessions", "truncated.jsonl")); got != prefix+`{"type":"event_msg","pay` {
		t.Fatal("source was modified")
	}
}

func TestImportConflictPoliciesAndPrivateIdempotentArchive(t *testing.T) {
	for _, policy := range []string{"skip", "theirs", "keep-both"} {
		t.Run(policy, func(t *testing.T) {
			base := canonicalTestDir(t)
			source, target := filepath.Join(base, "source"), filepath.Join(base, "target")
			roots := Roots{CodexHome: filepath.Join(base, "source-codex")}
			dest := Roots{CodexHome: filepath.Join(base, "target-codex")}
			relative := "sessions/a.jsonl"
			incoming := codexLog("a", source) + `{"type":"event_msg","message":"incoming"}` + "\n"
			existing := codexLog("a", target) + `{"type":"event_msg","message":"destination"}` + "\n"
			writeTest(t, filepath.Join(roots.CodexHome, relative), incoming)
			writeTest(t, filepath.Join(dest.CodexHome, relative), existing)
			bundle := filepath.Join(base, "bundle")
			if _, err := Export(source, bundle, roots); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(base, "private", "conflicts")
			options := ImportOptions{ConflictPolicy: policy, ConflictsDir: archive}
			preview, err := ValidateImportWithOptions(bundle, target, dest, options)
			if err != nil || preview.Conflicts != 1 {
				t.Fatalf("preflight: %#v %v", preview, err)
			}
			if _, err := os.Stat(archive); !os.IsNotExist(err) {
				t.Fatal("preflight created conflict archive")
			}
			result, err := ImportWithOptions(bundle, target, dest, options)
			if err != nil || result.Conflicts != 1 {
				t.Fatalf("import: %#v %v", result, err)
			}
			wanted := existing
			if policy == "theirs" {
				data, _ := rewrite([]byte(incoming), source, target)
				wanted = string(data)
			}
			if got := readTest(t, filepath.Join(dest.CodexHome, relative)); got != wanted {
				t.Fatalf("policy %s produced wrong content", policy)
			}
			if policy == "keep-both" {
				if len(result.PreservedPaths) != 1 {
					t.Fatal("no archived conflict path")
				}
				for _, path := range []string{archive, filepath.Dir(result.PreservedPaths[0])} {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0o700 {
						t.Fatalf("archive directory permissions: %v %v", info, err)
					}
				}
				first := readTest(t, result.PreservedPaths[0])
				if !strings.Contains(first, "incoming") {
					t.Fatal("incoming not archived")
				}
				again, err := ImportWithOptions(bundle, target, dest, options)
				if err != nil || len(again.PreservedPaths) != 1 || again.PreservedPaths[0] != result.PreservedPaths[0] {
					t.Fatalf("repeat: %#v %v", again, err)
				}
				entries, err := os.ReadDir(filepath.Dir(result.PreservedPaths[0]))
				if err != nil || len(entries) != 1 {
					t.Fatal("repeat created duplicate archive")
				}
			}
		})
	}
}

func TestClaudeSubdirectoryAndSameHomeHandoff(t *testing.T) {
	base := canonicalTestDir(t)
	source, target := filepath.Join(base, "source"), filepath.Join(base, "target")
	cwd := filepath.Join(source, "subdir")
	roots := Roots{CodexHome: filepath.Join(base, "codex"), ClaudeHome: filepath.Join(base, "claude")}
	codexRelative := "sessions/a.jsonl"
	claudeRelative := filepath.Join("projects", ProjectDirectory(cwd), "claude.jsonl")
	codex := codexLog("codex", source)
	claude := claudeLog("claude", cwd)
	writeTest(t, filepath.Join(roots.CodexHome, codexRelative), codex)
	writeTest(t, filepath.Join(roots.ClaudeHome, claudeRelative), claude)
	bundle := filepath.Join(base, "bundle")
	if _, err := Export(source, bundle, roots); err != nil {
		t.Fatal(err)
	}
	result, err := Import(bundle, target, roots)
	if err != nil || result.Skipped != 2 || result.Imported != 0 || result.Updated != 0 {
		t.Fatalf("same-home: %#v %v", result, err)
	}
	if got := readTest(t, filepath.Join(roots.CodexHome, codexRelative)); got != codex {
		t.Fatal("source Codex history changed")
	}
	if got := readTest(t, filepath.Join(roots.ClaudeHome, claudeRelative)); got != claude {
		t.Fatal("source Claude history changed")
	}
	destination := Roots{CodexHome: filepath.Join(base, "other-codex"), ClaudeHome: filepath.Join(base, "other-claude")}
	if _, err := Import(bundle, target, destination); err != nil {
		t.Fatal(err)
	}
	mapped := filepath.Join(destination.ClaudeHome, "projects", ProjectDirectory(filepath.Join(target, "subdir")), "claude.jsonl")
	if got := readTest(t, mapped); !strings.Contains(got, filepath.Join(target, "subdir")) {
		t.Fatal("Claude subdirectory missing")
	}
}

func TestSelectionAndCacheInvalidationWhenMtimeIsRestored(t *testing.T) {
	base := canonicalTestDir(t)
	project := filepath.Join(base, "project")
	roots := Roots{CodexHome: filepath.Join(base, "codex")}
	recent := filepath.Join(roots.CodexHome, "sessions", "recent.jsonl")
	old := filepath.Join(roots.CodexHome, "sessions", "old.jsonl")
	writeTest(t, recent, codexLog("recent", project))
	writeTest(t, old, codexLog("old", project))
	oldTime := time.Now().AddDate(0, 0, -30)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	defaultResult, err := DiscoverWithOptions(project, roots, Selection{})
	if err != nil || len(defaultResult.Sessions) != 1 || defaultResult.Sessions[0].ID != "recent" {
		t.Fatalf("default: %#v %v", defaultResult, err)
	}
	selected, err := DiscoverWithOptions(project, roots, Selection{IDs: []string{"old"}})
	if err != nil || len(selected.Sessions) != 1 || selected.Sessions[0].ID != "old" {
		t.Fatalf("explicit: %#v %v", selected, err)
	}
	all, err := DiscoverWithOptions(project, roots, Selection{All: true})
	if err != nil || len(all.Sessions) != 2 {
		t.Fatalf("all: %#v %v", all, err)
	}
	before, err := os.Stat(recent)
	if err != nil {
		t.Fatal(err)
	}
	// Same inode, size and mtime; ctime must invalidate the cache.
	writeTest(t, recent, codexLog("edited", project))
	if err := os.Chtimes(recent, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed, err := DiscoverWithOptions(project, roots, Selection{})
	if err != nil || len(changed.Sessions) != 1 || changed.Sessions[0].ID != "edited" || changed.Sessions[0].SHA256 == defaultResult.Sessions[0].SHA256 {
		t.Fatalf("stale cache: %#v %v", changed, err)
	}
	missing, err := DiscoverWithOptions(project, roots, Selection{IDs: []string{"missing"}})
	if err != nil || len(missing.Sessions) != 0 || len(missing.Warnings) != 1 {
		t.Fatalf("missing ID: %#v %v", missing, err)
	}
}

func TestProjectDirectoryMatchesClaudeUTF16AndLongPathEncoding(t *testing.T) {
	cases := []struct{ root, expected string }{
		{"/a/project.with_space", "-a-project-with-space"},
		{"/emoji/😀/project", "-emoji----project"},
		{"/" + strings.Repeat("a", 199), "-" + strings.Repeat("a", 199)},
		{"/" + strings.Repeat("a", 200), "-" + strings.Repeat("a", 199) + "-b6ymvl"},
		{"/" + strings.TrimSuffix(strings.Repeat("long😀segment/", 20), "/"), "-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-long--segment-lon-dx6sf8"},
	}
	for _, test := range cases {
		if got := ProjectDirectory(test.root); got != test.expected {
			t.Fatalf("encoding %q: want %q, got %q", test.root, test.expected, got)
		}
	}
}

func TestProjectAliasMetadataIsRemappedWithoutChangingMessages(t *testing.T) {
	base := canonicalTestDir(t)
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(alias, "project")
	target := filepath.Join(base, "target")
	roots := Roots{CodexHome: filepath.Join(base, "codex")}
	dest := Roots{CodexHome: filepath.Join(base, "destination")}
	log := codexLog("alias", source) + jsonLine(map[string]any{"type": "event_msg", "payload": map[string]any{"cwd": filepath.Join(source, "sub"), "message": source}})
	writeTest(t, filepath.Join(roots.CodexHome, "sessions", "alias.jsonl"), log)
	bundle := filepath.Join(base, "bundle")
	manifest, err := Export(source, bundle, roots)
	if err != nil || manifest.SourceRoot != filepath.Join(real, "project") {
		t.Fatalf("export alias: %#v %v", manifest, err)
	}
	if _, err := Import(bundle, target, dest); err != nil {
		t.Fatal(err)
	}
	imported := readTest(t, filepath.Join(dest.CodexHome, "sessions", "alias.jsonl"))
	if !strings.Contains(imported, `"cwd":"`+target+`"`) || !strings.Contains(imported, `"cwd":"`+target+`/sub"`) {
		t.Fatalf("alias metadata not remapped: %s", imported)
	}
	if !strings.Contains(imported, `"message":"`+source+`"`) {
		t.Fatal("message content changed")
	}
}

func TestNativeVersionFixturesExportAndImport(t *testing.T) {
	for _, test := range []struct{ agent, file, id string }{
		{"codex", "codex-0.163.0.jsonl", "01a142eb-2305-70e2-95af-a154bcb38695"},
		{"claude", "claude-2.1.301.jsonl", "425298b1-74d3-4ef4-9655-bca78e82b024"},
	} {
		t.Run(test.agent, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", test.file))
			if err != nil {
				t.Fatal(err)
			}
			id, cwd, err := metadata(data, test.agent)
			if err != nil || id != test.id || cwd != "/fixture/project" {
				t.Fatalf("metadata: %s %s %v", id, cwd, err)
			}
			base := canonicalTestDir(t)
			source := filepath.Join(base, "source")
			target := filepath.Join(base, "target")
			localized, err := rewrite(data, "/fixture/project", source)
			if err != nil {
				t.Fatal(err)
			}
			roots := Roots{CodexHome: filepath.Join(base, "codex"), ClaudeHome: filepath.Join(base, "claude")}
			destination := Roots{CodexHome: filepath.Join(base, "target-codex"), ClaudeHome: filepath.Join(base, "target-claude")}
			relative := filepath.Join("sessions", test.file)
			home := roots.CodexHome
			if test.agent == "claude" {
				relative = filepath.Join("projects", ProjectDirectory(source), test.file)
				home = roots.ClaudeHome
			}
			writeTest(t, filepath.Join(home, relative), string(localized))
			bundle := filepath.Join(base, "bundle")
			if _, err := Export(source, bundle, roots); err != nil {
				t.Fatal(err)
			}
			if got := readTest(t, filepath.Join(bundle, test.agent, relative)); got != string(localized) {
				t.Fatal("export changed native fixture bytes")
			}
			result, err := Import(bundle, target, destination)
			if err != nil || result.Imported != 1 {
				t.Fatalf("import: %#v %v", result, err)
			}
			roundtrip, err := rewrite(localized, source, target)
			if err != nil {
				t.Fatal(err)
			}
			destinationHome := destination.CodexHome
			if test.agent == "claude" {
				destinationHome = destination.ClaudeHome
				relative = filepath.Join("projects", ProjectDirectory(target), test.file)
			}
			if got := readTest(t, filepath.Join(destinationHome, relative)); got != string(roundtrip) {
				t.Fatal("native fixture import bytes differ")
			}
		})
	}
}

func TestDuplicateConflictContentIsArchivedOnce(t *testing.T) {
	base := canonicalTestDir(t)
	source, target := filepath.Join(base, "source"), filepath.Join(base, "target")
	roots := Roots{CodexHome: filepath.Join(base, "source-codex")}
	dest := Roots{CodexHome: filepath.Join(base, "target-codex")}
	for _, name := range []string{"a.jsonl", "b.jsonl"} {
		writeTest(t, filepath.Join(roots.CodexHome, "sessions", name), codexLog("same-id", source)+`{"message":"incoming"}`+"\n")
		writeTest(t, filepath.Join(dest.CodexHome, "sessions", name), codexLog("same-id", target)+`{"message":"destination"}`+"\n")
	}
	bundle := filepath.Join(base, "bundle")
	if _, err := Export(source, bundle, roots); err != nil {
		t.Fatal(err)
	}
	result, err := ImportWithOptions(bundle, target, dest, ImportOptions{ConflictsDir: filepath.Join(base, "conflicts")})
	if err != nil || result.Conflicts != 2 || len(result.PreservedPaths) != 1 {
		t.Fatalf("duplicate archive: %#v %v", result, err)
	}
}

func TestSourceAliasesSurviveAReceiverWithoutTheSourceSymlink(t *testing.T) {
	base := canonicalTestDir(t)
	real := filepath.Join(base, "source-real")
	if err := os.MkdirAll(filepath.Join(real, "project", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "source-only-alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(alias, "project")
	target := filepath.Join(base, "target")
	roots := Roots{ClaudeHome: filepath.Join(base, "claude")}
	dest := Roots{ClaudeHome: filepath.Join(base, "target-claude")}
	cwd := filepath.Join(source, "sub")
	relative := filepath.Join("projects", ProjectDirectory(cwd), "session.jsonl")
	log := claudeLog("alias", cwd) + jsonLine(map[string]any{"cwd": cwd, "writable_roots": []string{source}, "message": map[string]any{"path": cwd}})
	writeTest(t, filepath.Join(roots.ClaudeHome, relative), log)
	bundle := filepath.Join(base, "bundle")
	manifest, err := Export(source, bundle, roots)
	if err != nil || manifest.SourceAliases[source] != filepath.Join(real, "project") {
		t.Fatalf("export aliases: %#v %v", manifest.SourceAliases, err)
	}
	if got := readTest(t, filepath.Join(bundle, "claude", relative)); got != log {
		t.Fatal("alias export changed source bytes")
	}
	// Simulate receiving on another OS, where this source symlink never existed.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(bundle, target, dest); err != nil {
		t.Fatal(err)
	}
	imported := readTest(t, filepath.Join(dest.ClaudeHome, "projects", ProjectDirectory(filepath.Join(target, "sub")), "session.jsonl"))
	if !strings.Contains(imported, `"cwd":"`+target+`/sub"`) || !strings.Contains(imported, `"writable_roots":["`+target+`"]`) {
		t.Fatalf("cross-machine alias not remapped: %s", imported)
	}
	if !strings.Contains(imported, `"message":{"path":"`+cwd+`"}`) {
		t.Fatal("message path was modified")
	}
}

func TestSourceAliasesCannotEscapeTheManifestRoot(t *testing.T) {
	for _, aliases := range []map[string]string{
		{"/": "/source/project"},
		{"/alias": "/outside"},
		{"/alias": "/source/project/../outside"},
		{"relative": "/source/project"},
		{"/alias/../other": "/source/project"},
	} {
		if err := validateAliases(aliases, "/source/project"); err == nil {
			t.Fatalf("unsafe alias accepted: %#v", aliases)
		}
	}
	aliases := map[string]string{"/alias": "/source/project"}
	data := []byte(`{"cwd":"/alias/sub","path":"/alias-other/file","message":{"cwd":"/alias/sub"}}` + "\n")
	actual, err := rewriteAliases(data, "/source/project", "/destination", aliases)
	expected := `{"cwd":"/destination/sub","path":"/alias-other/file","message":{"cwd":"/alias/sub"}}` + "\n"
	if err != nil || string(actual) != expected {
		t.Fatalf("alias scope: %s %v", actual, err)
	}
}
