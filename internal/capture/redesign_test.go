package capture_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/packageotg"
	"github.com/peerproblem/onthego/internal/restore"
)

func TestInPlaceGitObjectsIncrementalAndUndo(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(base, "home"))
	source := filepath.Join(base, "source")
	mustMkdir(t, source)
	git(t, source, "init", "-b", "main")
	git(t, source, "config", "user.name", "Test")
	git(t, source, "config", "user.email", "test@example.invalid")
	history := make([]byte, 1<<20)
	if _, e := rand.Read(history); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(source, "historical.bin"), history, 0644)
	mustWrite(t, filepath.Join(source, "tracked.txt"), []byte("original\n"), 0644)
	mustWrite(t, filepath.Join(source, "stable.txt"), []byte("unchanged\n"), 0644)
	mustWrite(t, filepath.Join(source, ".gitignore"), []byte("node_modules/\n"), 0644)
	git(t, source, "add", ".")
	git(t, source, "commit", "-m", "historical fixture")
	git(t, source, "rm", "historical.bin")
	git(t, source, "commit", "-m", "remove historical payload")
	cfg := config.Default()
	if e := config.Save(source, cfg); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(source, "managed.txt"), []byte("original extra"), 0644)
	keys, e := identity.LoadOrCreate()
	if e != nil {
		t.Fatal(e)
	}
	secret, e := keys.Identity()
	if e != nil {
		t.Fatal(e)
	}
	first, e := capture.Create(ctx, capture.Options{Root: source, Config: cfg, Output: filepath.Join(base, "first.otg"), Keys: keys})
	if e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(base, "target")
	if _, e = restore.Package(ctx, first.Output, target, secret, keys.SigningPublic); e != nil {
		t.Fatal(e)
	}
	git(t, target, "config", "core.hooksPath", ".husky")
	git(t, target, "config", "user.name", "Local Name")
	git(t, target, "remote", "add", "local-only", "https://example.invalid/local.git")
	git(t, target, "config", "branch.main.remote", "local-only")
	git(t, target, "config", "branch.main.merge", "refs/heads/main")
	git(t, target, "update-ref", "refs/stash", "HEAD")
	mustWrite(t, filepath.Join(target, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nexit 9\n"), 0755)
	mustWrite(t, filepath.Join(target, "node_modules", "keep.txt"), []byte("ignored cache"), 0644)
	mustWrite(t, filepath.Join(target, "local-only.txt"), []byte("unmanaged"), 0644)
	beforeConfig, e := os.ReadFile(filepath.Join(target, ".git", "config"))
	if e != nil {
		t.Fatal(e)
	}
	beforeStable, _ := os.Stat(filepath.Join(target, "stable.txt"))
	beforeCache, _ := os.Stat(filepath.Join(target, "node_modules", "keep.txt"))
	beforeReflog, e := os.ReadFile(filepath.Join(target, ".git", "logs", "HEAD"))
	if e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(source, "tracked.txt"), []byte("staged\x00binary\xff\n"), 0755)
	if e = os.Chmod(filepath.Join(source, "tracked.txt"), 0755); e != nil {
		t.Fatal(e)
	}
	git(t, source, "add", "tracked.txt")
	mustWrite(t, filepath.Join(source, "tracked.txt"), []byte("unstaged\x00binary\xfe\n"), 0755)
	if e = os.Remove(filepath.Join(source, "managed.txt")); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(source, "new.txt"), []byte("new extra"), 0644)
	tips, e := gitx.Tips(ctx, target)
	if e != nil {
		t.Fatal(e)
	}
	second, e := capture.Create(ctx, capture.Options{Root: source, Config: cfg, Output: filepath.Join(base, "second.otg"), Keys: keys, RecipientTips: tips, ParentWIPCommit: first.Manifest.Git.WIPCommit, ParentSnapshotID: first.Manifest.SnapshotID})
	if e != nil {
		t.Fatal(e)
	}
	extracted := filepath.Join(base, "extracted")
	m, e := packageotg.Extract(second.Output, extracted, secret, keys.SigningPublic)
	if e != nil {
		t.Fatal(e)
	}
	bundle, e := os.ReadFile(filepath.Join(extracted, "repo.bundle"))
	if e != nil {
		t.Fatal(e)
	}
	if len(bundle) > 50<<10 {
		t.Fatalf("incremental bundle resent historical contents: %d bytes", len(bundle))
	}
	if !strings.Contains(string(bundle[:min(len(bundle), 500)]), "\n-") {
		t.Fatal("bundle has no prerequisite commit")
	}
	if _, e = restore.Config(ctx, extracted, m, target); e != nil {
		t.Fatal(e)
	}
	ancestor, e := gitx.IsAncestor(ctx, target, first.Manifest.Git.WIPCommit, second.Manifest.Git.WIPCommit)
	if e != nil || !ancestor {
		t.Fatalf("handoff lineage missing: %v %v", ancestor, e)
	}
	var managed []string
	for _, f := range first.Manifest.Files {
		managed = append(managed, f.RelativePath)
	}
	applied, e := restore.Apply(ctx, extracted, target, m, managed)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(applied.Backup, "refs/onthego/backup/") {
		t.Fatalf("not an object backup: %s", applied.Backup)
	}
	afterConfig, e := os.ReadFile(filepath.Join(target, ".git", "config"))
	if e != nil || string(afterConfig) != string(beforeConfig) {
		t.Fatalf("local Git configuration changed: %v", e)
	}
	afterStable, _ := os.Stat(filepath.Join(target, "stable.txt"))
	afterCache, _ := os.Stat(filepath.Join(target, "node_modules", "keep.txt"))
	if !os.SameFile(beforeStable, afterStable) || !os.SameFile(beforeCache, afterCache) {
		t.Fatal("unchanged tracked/ignored files were replaced")
	}
	for _, path := range []string{"local-only.txt", ".git/hooks/pre-commit"} {
		if _, e = os.Stat(filepath.Join(target, path)); e != nil {
			t.Fatal(e)
		}
	}
	afterReflog, e := os.ReadFile(filepath.Join(target, ".git", "logs", "HEAD"))
	if e != nil || !strings.Contains(string(afterReflog), string(beforeReflog)) {
		t.Fatalf("HEAD reflog was lost: %v", e)
	}
	if _, e = gitx.Run(ctx, target, "rev-parse", "refs/stash"); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(target, "managed.txt")); !os.IsNotExist(e) {
		t.Fatalf("previous managed deletion was ignored: %v", e)
	}
	assertGitOutput(t, ctx, source, target, "diff", "--cached", "--binary", "--full-index")
	assertGitOutput(t, ctx, source, target, "diff", "--binary", "--full-index")
	if _, e = restore.Undo(ctx, target, applied.Backup); e != nil {
		t.Fatal(e)
	}
	for path, want := range map[string]string{"tracked.txt": "original\n", "managed.txt": "original extra", "local-only.txt": "unmanaged"} {
		data, e := os.ReadFile(filepath.Join(target, path))
		if e != nil || string(data) != want {
			t.Fatalf("undo %s: %q %v", path, data, e)
		}
	}
	if _, e = os.Stat(filepath.Join(target, "new.txt")); !os.IsNotExist(e) {
		t.Fatalf("undo retained new managed file: %v", e)
	}
	status, e := gitx.Run(ctx, target, "diff", "--cached", "--exit-code")
	if e != nil {
		t.Fatalf("undo did not restore index: %s %v", status.Stdout, e)
	}
	backups, e := restore.Backups(ctx, target)
	if e != nil || len(backups) != 1 {
		t.Fatalf("undo inverse backup missing: %#v %v", backups, e)
	}
	if e = restore.PruneBackups(ctx, target, 0); e != nil {
		t.Fatal(e)
	}
	backups, e = restore.Backups(ctx, target)
	if e != nil || len(backups) != 0 {
		t.Fatalf("prune failed: %#v %v", backups, e)
	}
}

func TestSecretCandidateNamesAreNarrowAndVisible(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(t.TempDir(), "home"))
	git(t, root, "init")
	git(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	for _, path := range []string{"src/tokenizer.ts", "docs-secret-santa.md", "design.key", ".env.private", "private.pem", "id_custom", "certificate.p12"} {
		mustWrite(t, filepath.Join(root, path), []byte("fixture"), 0644)
	}
	cfg := config.Default()
	preview, e := capture.Preview(context.Background(), capture.Options{Root: root, Config: cfg})
	if e != nil {
		t.Fatal(e)
	}
	got := map[string]bool{}
	for _, f := range preview {
		got[f.RelativePath] = f.Excluded
	}
	for _, path := range []string{"src/tokenizer.ts", "docs-secret-santa.md", "design.key"} {
		if excluded, ok := got[path]; !ok || excluded {
			t.Fatalf("ordinary file omitted: %s %#v", path, preview)
		}
	}
	for _, path := range []string{".env.private", "private.pem", "id_custom", "certificate.p12"} {
		if !got[path] {
			t.Fatalf("secret exclusion not visible: %s %#v", path, preview)
		}
	}
	keys, e := identity.LoadOrCreate()
	if e != nil {
		t.Fatal(e)
	}
	result, e := capture.Create(context.Background(), capture.Options{Root: root, Config: cfg, Output: filepath.Join(t.TempDir(), "test.otg"), Keys: keys})
	if e != nil {
		t.Fatal(e)
	}
	if len(result.ExcludedFiles) != 4 {
		t.Fatalf("capture excluded file list missing: %#v", result.ExcludedFiles)
	}
	if len(result.Manifest.Files) != 3 {
		t.Fatalf("false positive secret filtering: %#v", result.Manifest.Files)
	}
}

func TestCaptureLinkedWorktreeUsesItsOwnIndex(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(base, "home"))
	root := filepath.Join(base, "main")
	mustMkdir(t, root)
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "user.email", "test@example.invalid")
	mustWrite(t, filepath.Join(root, "file.txt"), []byte("base"), 0644)
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "fixture")
	linked := filepath.Join(base, "linked")
	git(t, root, "worktree", "add", "-b", "feature", linked)
	mustWrite(t, filepath.Join(linked, "file.txt"), []byte("staged"), 0644)
	git(t, linked, "add", "file.txt")
	mustWrite(t, filepath.Join(linked, "file.txt"), []byte("unstaged"), 0644)
	cfg := config.Default()
	if e := config.Save(linked, cfg); e != nil {
		t.Fatal(e)
	}
	keys, e := identity.LoadOrCreate()
	if e != nil {
		t.Fatal(e)
	}
	created, e := capture.Create(ctx, capture.Options{Root: linked, Config: cfg, Output: filepath.Join(base, "snapshot.otg"), Keys: keys})
	if e != nil {
		t.Fatal(e)
	}
	secret, e := keys.Identity()
	if e != nil {
		t.Fatal(e)
	}
	extracted := filepath.Join(base, "payload")
	m, e := packageotg.Extract(created.Output, extracted, secret, keys.SigningPublic)
	if e != nil {
		t.Fatal(e)
	}
	// Applying feature to main would move a branch checked out elsewhere.
	if _, e = restore.Apply(ctx, extracted, root, m, nil); e == nil || !strings.Contains(e.Error(), "another linked worktree") {
		t.Fatalf("unsafe linked branch update allowed: %v", e)
	}
	metadata, e := os.ReadFile(filepath.Join(linked, ".git"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = restore.Apply(ctx, extracted, linked, m, nil); e != nil {
		t.Fatal(e)
	}
	after, e := os.ReadFile(filepath.Join(linked, ".git"))
	if e != nil || string(after) != string(metadata) {
		t.Fatalf("linked worktree pointer changed: %v", e)
	}
	if data, e := os.ReadFile(filepath.Join(root, "file.txt")); e != nil || string(data) != "base" {
		t.Fatalf("main worktree was modified: %q %v", data, e)
	}
	assertGitOutput(t, ctx, linked, linked, "diff", "--cached", "--binary")
}

func TestApplyTrackedDirectoryTransitionsAndBackupRetention(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(base, "home"))
	source := filepath.Join(base, "source")
	mustMkdir(t, source)
	git(t, source, "init", "-b", "main")
	git(t, source, "config", "user.name", "Test")
	git(t, source, "config", "user.email", "test@example.invalid")
	mustWrite(t, filepath.Join(source, "directory", "file.txt"), []byte("original"), 0644)
	mustWrite(t, filepath.Join(source, "leaf.txt"), []byte("leaf"), 0644)
	git(t, source, "add", ".")
	git(t, source, "commit", "-m", "fixture")
	cfg := config.Default()
	if e := config.Save(source, cfg); e != nil {
		t.Fatal(e)
	}
	keys, e := identity.LoadOrCreate()
	if e != nil {
		t.Fatal(e)
	}
	secret, e := keys.Identity()
	if e != nil {
		t.Fatal(e)
	}
	first, e := capture.Create(ctx, capture.Options{Root: source, Config: cfg, Output: filepath.Join(base, "first.otg"), Keys: keys})
	if e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(base, "target")
	if _, e = restore.Package(ctx, first.Output, target, secret, keys.SigningPublic); e != nil {
		t.Fatal(e)
	}
	if e = os.RemoveAll(filepath.Join(source, "directory")); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(source, "directory"), []byte("now a file"), 0644)
	if e = os.Remove(filepath.Join(source, "leaf.txt")); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(source, "leaf.txt", "nested"), []byte("now a directory"), 0644)
	git(t, source, "add", ".")
	second, e := capture.Create(ctx, capture.Options{Root: source, Config: cfg, Output: filepath.Join(base, "second.otg"), Keys: keys})
	if e != nil {
		t.Fatal(e)
	}
	extracted := filepath.Join(base, "payload")
	m, e := packageotg.Extract(second.Output, extracted, secret, keys.SigningPublic)
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < restore.DefaultBackupLimit+2; n++ {
		if _, e = restore.Apply(ctx, extracted, target, m, []string{config.FileName}); e != nil {
			t.Fatalf("apply %d: %v", n, e)
		}
	}
	for path, want := range map[string]string{"directory": "now a file", "leaf.txt/nested": "now a directory"} {
		data, e := os.ReadFile(filepath.Join(target, path))
		if e != nil || string(data) != want {
			t.Fatalf("transition %s: %q %v", path, data, e)
		}
	}
	backups, e := restore.Backups(ctx, target)
	if e != nil || len(backups) != restore.DefaultBackupLimit {
		t.Fatalf("unbounded object backup refs: %d %v", len(backups), e)
	}
	legacy, e := filepath.Glob(filepath.Join(base, ".onthego-backup-*"))
	if e != nil || len(legacy) != 0 {
		t.Fatalf("directory backups remain: %v %v", legacy, e)
	}
}

func TestFreshApplyFailureIsCleanAndRetryable(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(base, "home"))
	source := filepath.Join(base, "source")
	mustMkdir(t, source)
	git(t, source, "init", "-b", "main")
	git(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	cfg := config.Default()
	if e := config.Save(source, cfg); e != nil {
		t.Fatal(e)
	}
	keys, e := identity.LoadOrCreate()
	if e != nil {
		t.Fatal(e)
	}
	secret, e := keys.Identity()
	if e != nil {
		t.Fatal(e)
	}
	result, e := capture.Create(ctx, capture.Options{Root: source, Config: cfg, Output: filepath.Join(base, "snapshot.otg"), Keys: keys})
	if e != nil {
		t.Fatal(e)
	}
	extracted := filepath.Join(base, "payload")
	m, e := packageotg.Extract(result.Output, extracted, secret, keys.SigningPublic)
	if e != nil {
		t.Fatal(e)
	}
	realGit, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	bin := filepath.Join(base, "bin")
	mustMkdir(t, bin)
	script := "#!/bin/sh\nif [ \"$1\" = read-tree ] && [ \"$ONTHEGO_TEST_FAIL_READ_TREE\" = 1 ]; then echo injected failure >&2; exit 1; fi\nexec '" + strings.ReplaceAll(realGit, "'", "'\\''") + "' \"$@\"\n"
	mustWrite(t, filepath.Join(bin, "git"), []byte(script), 0755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ONTHEGO_TEST_FAIL_READ_TREE", "1")
	target := filepath.Join(base, "target")
	if _, e = restore.Apply(ctx, extracted, target, m, nil); e == nil {
		t.Fatal("injected read-tree failure ignored")
	}
	if _, e = os.Lstat(target); !os.IsNotExist(e) {
		t.Fatalf("failed fresh restore left destination behind: %v", e)
	}
	t.Setenv("ONTHEGO_TEST_FAIL_READ_TREE", "0")
	if _, e = restore.Apply(ctx, extracted, target, m, nil); e != nil {
		t.Fatalf("retry failed: %v", e)
	}
}

func TestSHA256RepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(base, "home"))
	source := filepath.Join(base, "source")
	mustMkdir(t, source)
	git(t, source, "init", "--object-format=sha256", "-b", "main")
	git(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	cfg := config.Default()
	if e := config.Save(source, cfg); e != nil {
		t.Fatal(e)
	}
	keys, e := identity.LoadOrCreate()
	if e != nil {
		t.Fatal(e)
	}
	secret, e := keys.Identity()
	if e != nil {
		t.Fatal(e)
	}
	result, e := capture.Create(ctx, capture.Options{Root: source, Config: cfg, Output: filepath.Join(base, "snapshot.otg"), Keys: keys})
	if e != nil {
		t.Fatal(e)
	}
	if len(result.Manifest.SourceGitHead) != 64 {
		t.Fatal("fixture is not SHA256")
	}
	target := filepath.Join(base, "target")
	if _, e = restore.Package(ctx, result.Output, target, secret, keys.SigningPublic); e != nil {
		t.Fatal(e)
	}
	assertGitOutput(t, ctx, source, target, "rev-parse", "HEAD")
}

func TestRawWorktreeBytesBypassAttributesAndGlobalCleanFilters(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", filepath.Join(base, "home"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(base, "global.gitconfig"))
	source := filepath.Join(base, "source")
	mustMkdir(t, source)
	git(t, source, "init", "-b", "main")
	git(t, source, "config", "user.name", "Test")
	git(t, source, "config", "user.email", "test@example.invalid")
	mustWrite(t, filepath.Join(source, ".gitattributes"), []byte("*.txt text eol=lf\n*.raw filter=onthego-test\n"), 0644)
	mustWrite(t, filepath.Join(source, "tracked.txt"), []byte("base\n"), 0644)
	mustWrite(t, filepath.Join(source, "tracked.raw"), []byte("base raw\n"), 0644)
	git(t, source, "add", ".")
	git(t, source, "commit", "-m", "fixture")
	mustWrite(t, filepath.Join(source, "tracked.txt"), []byte("staged\r\n"), 0644)
	git(t, source, "add", "tracked.txt")
	names := map[string][]byte{"tracked.txt": []byte("worktree\r\nraw\r\n"), "tracked.raw": []byte("unfiltered tracked\r\n"), "new.raw": []byte("unfiltered extra\r\n"), "한글\nquote\"tab\t.txt": []byte("unicode filename\r\n"), "-leading.txt": []byte("leading dash\r\n")}
	for name, data := range names {
		mustWrite(t, filepath.Join(source, name), data, 0644)
	}
	// More than one descriptor batch exercises fixed-size batching without a
	// process per file and makes unusual filenames share a batch with normal ones.
	for i := 0; i < 70; i++ {
		name := fmt.Sprintf("extra-%02d.txt", i)
		data := []byte("batch raw\r\n")
		names[name] = data
		mustWrite(t, filepath.Join(source, name), data, 0644)
	}
	if e := os.Symlink("tracked.txt", filepath.Join(source, "local-link")); e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(base, "filter-executed")
	t.Setenv("ONTHEGO_FILTER_MARKER", marker)
	git(t, source, "config", "--global", "filter.onthego-test.clean", `printf invoked >> "$ONTHEGO_FILTER_MARKER"; tr a-z A-Z`)
	git(t, source, "config", "--global", "filter.onthego-test.required", "true")
	cfg := config.Default()
	if e := config.Save(source, cfg); e != nil {
		t.Fatal(e)
	}
	originalIndex, e := os.ReadFile(filepath.Join(source, ".git", "index"))
	if e != nil {
		t.Fatal(e)
	}
	keys, e := identity.LoadOrCreate()
	if e != nil {
		t.Fatal(e)
	}
	secret, e := keys.Identity()
	if e != nil {
		t.Fatal(e)
	}
	result, e := capture.Create(ctx, capture.Options{Root: source, Config: cfg, Output: filepath.Join(base, "snapshot.otg"), Keys: keys})
	if e != nil {
		t.Fatal(e)
	}
	afterIndex, e := os.ReadFile(filepath.Join(source, ".git", "index"))
	if e != nil || !bytes.Equal(originalIndex, afterIndex) {
		t.Fatalf("capture changed source staged index: %v", e)
	}
	if _, e = os.Stat(marker); !os.IsNotExist(e) {
		t.Fatalf("capture executed global clean filter: %v", e)
	}
	target := filepath.Join(base, "target")
	if _, e = restore.Package(ctx, result.Output, target, secret, keys.SigningPublic); e != nil {
		t.Fatal(e)
	}
	for name, want := range names {
		got, e := os.ReadFile(filepath.Join(target, name))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatalf("raw bytes changed for %q: %q != %q, %v", name, got, want, e)
		}
	}
	if link, e := os.Readlink(filepath.Join(target, "local-link")); e != nil || link != "tracked.txt" {
		t.Fatalf("symlink bytes changed: %q %v", link, e)
	}
	staged, e := gitx.Run(ctx, target, "show", ":tracked.txt")
	if e != nil || string(staged.Stdout) != "staged\n" {
		t.Fatalf("staged normalization changed: %q %v", staged.Stdout, e)
	}
	if _, e = os.Stat(marker); !os.IsNotExist(e) {
		t.Fatalf("restore executed global clean filter: %v", e)
	}
	before, e := capture.Fingerprint(ctx, capture.Options{Root: source, Config: cfg})
	if e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(source, "tracked.txt"), []byte("worktree\nraw\n"), 0644)
	after, e := capture.Fingerprint(ctx, capture.Options{Root: source, Config: cfg})
	if e != nil {
		t.Fatal(e)
	}
	if before == after {
		t.Fatal("raw CRLF-only workspace change was invisible to fingerprint")
	}
	if _, e = os.Stat(marker); !os.IsNotExist(e) {
		t.Fatalf("fingerprint executed global clean filter: %v", e)
	}
}
