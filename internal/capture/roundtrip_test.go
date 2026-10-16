package capture_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/restore"
)

func TestNamedEnvIsExcludedFromCapture(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	mustWrite(t, filepath.Join(root, "vps.env"), []byte("PASSWORD=test-only\n"), 0o600)
	mustWrite(t, filepath.Join(root, "notes.txt"), []byte("safe\n"), 0o644)
	mustWrite(t, filepath.Join(root, ".claude", ".credentials.json"), []byte("authentication material"), 0o600)
	cfg := config.Default()
	cfg.Include = nil
	files, err := capture.Preview(context.Background(), capture.Options{Root: root, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || !files[0].Excluded || files[1].RelativePath != "notes.txt" || !files[2].Excluded {
		t.Fatalf("unexpected captured files: %#v", files)
	}
	_, err = capture.Preview(context.Background(), capture.Options{Root: root, Config: cfg, Include: []string{"vps.env"}})
	if err == nil || !strings.Contains(err.Error(), "secret candidate") {
		t.Fatalf("expected explicit include to require secret opt-in, got %v", err)
	}
	_, err = capture.Preview(context.Background(), capture.Options{Root: root, Config: cfg, IncludeSecret: []string{".claude/.credentials.json"}})
	if err == nil {
		t.Fatal("Claude authentication material must never be selected")
	}
}

func TestCaptureRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(base, "data"))
	root := filepath.Join(base, "source")
	mustMkdir(t, filepath.Join(root, "evidence"))
	mustWrite(t, filepath.Join(root, ".gitignore"), []byte("evidence/ignored.bin\n"), 0o644)
	mustWrite(t, filepath.Join(root, "tracked.txt"), []byte("base\n"), 0o644)
	mustWrite(t, filepath.Join(root, "deleted.txt"), []byte("delete me\n"), 0o644)
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.name", "ONTHEGO Test")
	git(t, root, "config", "user.email", "test@example.invalid")
	git(t, root, "add", ".gitignore", "tracked.txt", "deleted.txt")
	git(t, root, "commit", "-m", "fixture")
	git(t, root, "checkout", "-b", "feature/work")
	git(t, root, "branch", "unpushed-work")
	git(t, root, "tag", "local-tag")
	git(t, root, "update-ref", "refs/stash", "HEAD")
	git(t, root, "remote", "add", "origin", "https://private-token@example.invalid/owner/project.git?token=private")

	mustWrite(t, filepath.Join(root, "tracked.txt"), []byte("staged\n"), 0o644)
	git(t, root, "add", "tracked.txt")
	mustWrite(t, filepath.Join(root, "tracked.txt"), []byte("unstaged\n"), 0o644)
	mustWrite(t, filepath.Join(root, "added.txt"), []byte("new staged file\n"), 0o644)
	git(t, root, "add", "added.txt")
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "한국어-메모.txt"), []byte("작업 맥락\n"), 0o755)
	binary := []byte{0, 1, 2, 0xff, 0, 7}
	mustWrite(t, filepath.Join(root, "evidence", "ignored.bin"), binary, 0o644)

	cfg := config.Default()
	cfg.Include = []string{config.FileName, "evidence/ignored.bin"}
	if err := config.Save(root, cfg); err != nil {
		t.Fatal(err)
	}
	contextDir := filepath.Join(base, "context")
	mustMkdir(t, contextDir)
	mustWrite(t, filepath.Join(contextDir, "session.jsonl"), []byte("{\"message\":\"redacted context\"}\n"), 0o600)
	mustWrite(t, filepath.Join(contextDir, "HANDOFF.md"), []byte("# Handoff\n"), 0o600)
	mustWrite(t, filepath.Join(contextDir, "context-envelope.json"), []byte(`{"schema_version":1,"id":"ctx-1","project_id":"`+cfg.ProjectID+`","agent_kind":"codex","session_id":"session-1"}`), 0o600)

	keys, err := identity.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(base, "snapshot.otg")
	result, err := capture.Create(ctx, capture.Options{Root: root, Config: cfg, Output: packagePath, ContextDir: contextDir, OnthegoVersion: "test", Keys: keys, ParentSnapshotID: "previous"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Manifest.ParentSnapshotIDs, ",") != "previous" {
		t.Fatalf("snapshot retained transitive ancestry: %#v", result.Manifest.ParentSnapshotIDs)
	}
	ageIdentity, err := keys.Identity()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(base, "restored")
	if _, err := restore.Package(ctx, packagePath, destination, ageIdentity, keys.SigningPublic); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"symbolic-ref", "--short", "HEAD"}, {"for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags", "refs/stash"}} {
		assertGitOutput(t, ctx, root, destination, args...)
	}
	origin, err := gitx.Run(ctx, destination, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(string(origin.Stdout)) != "https://example.invalid/owner/project.git" {
		t.Fatalf("restored origin is not portable and credential-free: %s %v", origin.Stdout, err)
	}
	before, err := capture.Fingerprint(ctx, capture.Options{Root: root, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	after, err := capture.Fingerprint(ctx, capture.Options{Root: destination, Config: cfg})
	if err != nil || before != after {
		t.Fatalf("workspace fingerprint changed after roundtrip: %s %s %v", before, after, err)
	}

	assertGitOutput(t, ctx, root, destination, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv")
	assertGitOutput(t, ctx, root, destination, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv")
	for _, path := range []string{"tracked.txt", "added.txt", "한국어-메모.txt", "evidence/ignored.bin", config.FileName} {
		left, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(destination, path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s differs after restore", path)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted tracked file was restored: %v", err)
	}
	info, err := os.Stat(filepath.Join(destination, "한국어-메모.txt"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("executable mode was not preserved: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".onthego.local", "context")); !os.IsNotExist(err) {
		t.Fatalf("plaintext context survived restore: %v", err)
	}
}

func TestFingerprintDetectsUntrackedAndIgnoredChanges(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	mustWrite(t, filepath.Join(root, ".gitignore"), []byte("local/\n"), 0o644)
	mustWrite(t, filepath.Join(root, "local", "cache.txt"), []byte("original"), 0o644)
	cfg := config.Default()
	cfg.Include = []string{"local"}
	options := capture.Options{Root: root, Config: cfg}
	fingerprint := func() string {
		t.Helper()
		value, err := capture.Fingerprint(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	initial := fingerprint()
	mustWrite(t, filepath.Join(root, "new.txt"), []byte("new work"), 0o644)
	withNew := fingerprint()
	if initial == withNew {
		t.Fatal("new untracked file did not change fingerprint")
	}
	mustWrite(t, filepath.Join(root, "local", "cache.txt"), []byte("updated"), 0o644)
	withIgnored := fingerprint()
	if withNew == withIgnored {
		t.Fatal("explicitly included ignored file did not change fingerprint")
	}
	mustWrite(t, filepath.Join(root, ".onthego.local", "context", "session.jsonl"), []byte("generated"), 0o600)
	if withIgnored != fingerprint() {
		t.Fatal("restored internal context changed workspace fingerprint")
	}
}

func TestCaptureRejectsGitMetadataAndSymlinkAncestors(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "notes.txt"), []byte("outside workspace"), 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	for _, include := range []string{".git/config", "outside/notes.txt"} {
		t.Run(include, func(t *testing.T) {
			cfg := config.Default()
			cfg.Include = []string{include}
			_, err := capture.Preview(context.Background(), capture.Options{Root: root, Config: cfg})
			if err == nil {
				t.Fatalf("unsafe include %s was accepted", include)
			}
		})
	}
}

func TestDetachedRestoreHasNoTemporaryRemote(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(base, "data"))
	root := filepath.Join(base, "source")
	mustMkdir(t, root)
	git(t, root, "init")
	git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	git(t, root, "checkout", "--detach")
	keys, err := identity.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(base, "snapshot.otg")
	if _, err := capture.Create(context.Background(), capture.Options{Root: root, Config: config.Default(), Output: packagePath, Keys: keys}); err != nil {
		t.Fatal(err)
	}
	ageIdentity, err := keys.Identity()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(base, "restored")
	if _, err := restore.Package(context.Background(), packagePath, destination, ageIdentity, keys.SigningPublic); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(context.Background(), destination, "symbolic-ref", "HEAD"); err == nil {
		t.Fatal("detached HEAD unexpectedly became a branch")
	}
	remotes, err := gitx.Run(context.Background(), destination, "remote")
	if err != nil || len(remotes.Stdout) != 0 {
		t.Fatalf("temporary bundle remote survived restore: %s %v", remotes.Stdout, err)
	}
}

func assertGitOutput(t *testing.T, ctx context.Context, left, right string, args ...string) {
	t.Helper()
	l, err := gitx.Run(ctx, left, args...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := gitx.Run(ctx, right, args...)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(l.Stdout, r.Stdout) {
		t.Fatalf("git %v output differs\nsource:\n%s\nrestored:\n%s", args, l.Stdout, r.Stdout)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
