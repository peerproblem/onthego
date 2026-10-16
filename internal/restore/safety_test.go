package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func safetyRepo(t *testing.T) (string, func(...string) string) {
	t.Helper()
	t.Setenv("ONTHEGO_HOME", filepath.Join(t.TempDir(), "home"))
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		r, err := gitx.Run(context.Background(), root, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(r.Stdout))
	}
	run("init", "-b", "main")
	run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	return root, run
}

func safetyTree(t *testing.T, root string, paths map[string]string) string {
	t.Helper()
	var input strings.Builder
	for path, body := range paths {
		r, err := gitx.RunInput(context.Background(), root, nil, []byte(body), "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		input.WriteString("100644 blob " + strings.TrimSpace(string(r.Stdout)) + "\t" + path + "\x00")
	}
	r, err := gitx.RunInput(context.Background(), root, nil, []byte(input.String()), "mktree", "-z")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(r.Stdout))
}

func TestCollisionPreflightDoesNotChangeWorkingFiles(t *testing.T) {
	for _, pair := range [][2]string{{"Foo.txt", "foo.txt"}, {"caf\u00e9.txt", "cafe\u0301.txt"}} {
		t.Run(pair[0], func(t *testing.T) {
			root, _ := safetyRepo(t)
			if err := os.WriteFile(filepath.Join(root, pair[0]), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			first, err := os.Lstat(filepath.Join(root, pair[0]))
			if err != nil {
				t.Fatal(err)
			}
			second, err := os.Lstat(filepath.Join(root, pair[1]))
			if err != nil || !os.SameFile(first, second) {
				t.Skip("filesystem distinguishes fixture names")
			}
			tree := safetyTree(t, root, map[string]string{pair[0]: "UPPER", pair[1]: "lower"})
			entries, err := gitx.Tree(context.Background(), root, tree)
			if err != nil {
				t.Fatal(err)
			}
			desired := map[string]gitx.TreeEntry{}
			for _, entry := range entries {
				desired[entry.Path] = entry
			}
			err = checkCollisions(context.Background(), root, map[string]bool{pair[0]: true}, desired)
			if err == nil || !strings.Contains(err.Error(), "paths collide on this filesystem") {
				t.Fatalf("collision not rejected: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(root, pair[0])); err != nil || string(data) != "original" {
				t.Fatalf("working file changed: %q, %v", data, err)
			}
		})
	}
}

func TestCaseOnlyRenameReplacesManagedFile(t *testing.T) {
	root, run := safetyRepo(t)
	if err := os.WriteFile(filepath.Join(root, "Foo.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	tree := safetyTree(t, root, map[string]string{"foo.txt": "new"})
	empty := run("rev-parse", "HEAD^{tree}")
	state := gitx.WIP{Head: run("rev-parse", "HEAD"), Branch: "main", IndexTree: empty, WorktreeTree: empty, UntrackedTree: tree}
	desired, err := desiredEntries(context.Background(), root, state)
	if err != nil {
		t.Fatal(err)
	}
	old := map[string]bool{"Foo.txt": true}
	if err = checkCollisions(context.Background(), root, old, desired); err != nil {
		t.Fatal(err)
	}
	if err = applyState(context.Background(), root, state, nil, old, nil, ""); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "foo.txt")); err != nil || string(data) != "new" {
		t.Fatalf("rename failed: %q, %v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "Foo.txt" {
			t.Fatal("old spelling remains")
		}
	}
}

func TestAtomicReplacementPreservesTargetOnSymlinkFailure(t *testing.T) {
	root, run := safetyRepo(t)
	path := filepath.Join(root, "target")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	// An overlong symlink target fails after the old implementation removes target.
	link := strings.Repeat("a", 128*1024)
	hash := sha256.Sum256([]byte(link))
	secretRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretRoot, "secret"), []byte(link), 0600); err != nil {
		t.Fatal(err)
	}
	empty := run("rev-parse", "HEAD^{tree}")
	state := gitx.WIP{Head: run("rev-parse", "HEAD"), Branch: "main", IndexTree: empty, WorktreeTree: empty, UntrackedTree: empty}
	files := []snapshot.File{{RelativePath: "target", ArchivePath: "secret", Kind: "symlink", Secret: true, SHA256: hex.EncodeToString(hash[:]), LinkTarget: link}}
	if err := applyState(context.Background(), root, state, files, map[string]bool{"target": true}, nil, secretRoot); err == nil {
		t.Fatal("expected oversized symlink failure")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
		t.Fatalf("failed replacement lost target: %q, %v", data, err)
	}
}

func TestFilesystemCollisionErrorIsTyped(t *testing.T) {
	root, _ := safetyRepo(t)
	err := checkCollisions(context.Background(), root, nil, map[string]gitx.TreeEntry{"file": {Path: "file"}, "file/child": {Path: "file/child"}})
	if err == nil || otgerror.Public(err).Code != otgerror.CodeRestoreFailed {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFilesystemPathKeys(t *testing.T) {
	for _, tc := range []struct {
		name            string
		fold, normalize bool
		first, second   string
		equal           bool
	}{
		{"sensitive case", false, false, "Foo", "foo", false},
		{"sensitive normalization", false, false, "caf\u00e9", "cafe\u0301", false},
		{"fold case", true, false, "Foo", "foo", true},
		{"normalization", false, true, "caf\u00e9", "cafe\u0301", true},
		{"fold and normalization", true, true, "CAF\u00c9", "cafe\u0301", true},
		{"normalization retains case", false, true, "CAF\u00c9", "cafe\u0301", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := filesystemPathKey(tc.fold, tc.normalize)
			if got := key(tc.first) == key(tc.second); got != tc.equal {
				t.Fatalf("equality = %v, want %v", got, tc.equal)
			}
		})
	}
}

func TestFilesystemParentAliasesRejected(t *testing.T) {
	desired := map[string]gitx.TreeEntry{"DIR": {Path: "DIR"}, "dir/file": {Path: "dir/file"}}
	if err := checkFilesystemPaths(desired, filesystemPathKey(true, false)); err == nil {
		t.Fatal("case-aliased file ancestor accepted")
	}
}

func TestRetainedPathsPreserveOnlyUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	root, run := safetyRepo(t)
	for _, path := range []string{"notes.txt", "deleted.txt", "tracked.txt"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "tracked.txt")
	run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "tracked fixture")
	old, err := managedBefore(ctx, root, []string{"notes.txt", "deleted.txt"})
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := retainUnmanagedPaths(ctx, root, []string{"notes.txt", "tracked.txt"}, nil, old)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != "No longer managed; left in place: notes.txt" {
		t.Fatalf("warnings = %#v", warnings)
	}
	if old["notes.txt"] || !old["deleted.txt"] || !old["tracked.txt"] {
		t.Fatalf("incorrect remaining managed files: %#v", old)
	}
	empty := safetyTree(t, root, nil)
	state := gitx.WIP{Head: run("rev-parse", "HEAD"), Branch: "main", IndexTree: empty, WorktreeTree: empty, UntrackedTree: empty}
	if err = applyState(ctx, root, state, nil, old, nil, ""); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "notes.txt")); err != nil || string(data) != "notes.txt" {
		t.Fatalf("retained file changed: %q %v", data, err)
	}
	for _, path := range []string{"deleted.txt", "tracked.txt"} {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("deletion did not propagate for %s: %v", path, err)
		}
	}
}

func TestRetainedPathsRejectInvalidManifestBeforeChangingManagedSet(t *testing.T) {
	root, _ := safetyRepo(t)
	for _, paths := range [][]string{{"../outside"}, {"same", "same"}, {"desired"}} {
		old := map[string]bool{"same": true}
		_, err := retainUnmanagedPaths(context.Background(), root, paths, map[string]gitx.TreeEntry{"desired": {Path: "desired"}}, old)
		if err == nil || otgerror.Public(err).Code != otgerror.CodePackageInvalid {
			t.Fatalf("invalid paths %q accepted: %v", paths, err)
		}
		if !old["same"] {
			t.Fatal("failed validation changed managed paths")
		}
	}
}

func TestWrittenFileIdentityDefense(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.WriteFile(first, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	a, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Lstat(second)
	if err != nil {
		t.Fatal(err)
	}
	if err = checkWrittenAlias("second", b, []writtenPath{{path: "first", info: a}}); err == nil {
		t.Fatal("distinct paths resolving to same file accepted")
	}
}

func TestAtomicReplacementPreservesTargetAfterPartialWriteFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	injected := &os.PathError{Op: "write", Path: "staged replacement", Err: os.ErrPermission}
	err := replaceFileStaged(path, 0600, false, "", func(file *os.File) error {
		if _, err := file.Write([]byte("partial")); err != nil {
			t.Fatal(err)
		}
		return injected
	})
	if err != injected {
		t.Fatalf("failure not returned: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original" {
		t.Fatalf("failed write damaged target: %q %v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" {
		t.Fatalf("temporary file leaked: %#v", entries)
	}
}

func TestApplyAndUndoRejectCollidingTreesBeforeChangingWorkspace(t *testing.T) {
	for _, pair := range [][2]string{{"Foo.txt", "foo.txt"}, {"caf\u00e9.txt", "cafe\u0301.txt"}} {
		t.Run(pair[0], func(t *testing.T) {
			ctx := context.Background()
			root, run := safetyRepo(t)
			if err := os.WriteFile(filepath.Join(root, pair[0]), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			key, err := pathKeyer(root)
			if err != nil {
				t.Fatal(err)
			}
			if key(pair[0]) != key(pair[1]) {
				t.Skip("filesystem distinguishes fixture names")
			}
			tree := safetyTree(t, root, map[string]string{pair[0]: "UPPER", pair[1]: "lower"})
			head, empty := run("rev-parse", "HEAD"), run("rev-parse", "HEAD^{tree}")
			index, err := gitx.CommitTree(ctx, root, empty, []string{head}, "index")
			if err != nil {
				t.Fatal(err)
			}
			extras, err := gitx.CommitTree(ctx, root, empty, nil, "extras")
			if err != nil {
				t.Fatal(err)
			}
			commit, err := gitx.CommitTree(ctx, root, tree, []string{head, index, extras}, "collision fixture")
			if err != nil {
				t.Fatal(err)
			}
			state := gitx.WIP{Head: head, Branch: "main", IndexTree: empty, WorktreeTree: tree, UntrackedTree: empty, Commit: commit}
			payload := t.TempDir()
			if err = gitx.Bundle(ctx, root, filepath.Join(payload, "repo.bundle"), commit, nil); err != nil {
				t.Fatal(err)
			}
			manifest := snapshot.Manifest{SchemaVersion: snapshot.SchemaVersion, SourceGitHead: head, SourceBranch: "main", Git: snapshot.Git{WIPCommit: commit, IndexTree: empty, WorktreeTree: tree, UntrackedTree: empty}}
			run("read-tree", empty)
			beforeIndex, err := os.ReadFile(filepath.Join(root, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			assertUnchanged := func() {
				t.Helper()
				if data, err := os.ReadFile(filepath.Join(root, pair[0])); err != nil || string(data) != "original" {
					t.Fatalf("working file changed: %q %v", data, err)
				}
				if run("rev-parse", "HEAD") != head {
					t.Fatal("HEAD changed")
				}
				afterIndex, err := os.ReadFile(filepath.Join(root, ".git", "index"))
				if err != nil || string(afterIndex) != string(beforeIndex) {
					t.Fatal("index changed")
				}
			}
			if _, err = Apply(ctx, payload, root, manifest, []string{pair[0]}); err == nil || !strings.Contains(err.Error(), "paths collide on this filesystem") {
				t.Fatalf("Apply accepted colliding tree: %v", err)
			}
			assertUnchanged()
			body, err := json.Marshal(backupState{WIP: state, AfterPaths: []string{pair[0]}})
			if err != nil {
				t.Fatal(err)
			}
			backup, err := gitx.CommitTree(ctx, root, tree, []string{commit}, string(body))
			if err != nil {
				t.Fatal(err)
			}
			ref := backupPrefix(root) + "collision"
			run("update-ref", ref, backup)
			if _, err = Undo(ctx, root, ref); err == nil || !strings.Contains(err.Error(), "paths collide on this filesystem") {
				t.Fatalf("Undo accepted colliding tree: %v", err)
			}
			assertUnchanged()
			run("rev-parse", ref)
		})
	}
}

func TestIncomingConfigRejectsInvalidPolicyAsPackageError(t *testing.T) {
	for _, policy := range []string{
		"schema_version: 2\nproject_id: safety-project\nagent: typo\n",
		"schema_version: 2\nproject_id: safety-project\nagent: auto\nunknown: true\n",
	} {
		ctx := context.Background()
		root, run := safetyRepo(t)
		path := filepath.Join(root, ".onthego.yaml")
		if err := os.WriteFile(path, []byte(policy), 0600); err != nil {
			t.Fatal(err)
		}
		run("add", ".onthego.yaml")
		run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "policy fixture")
		state, err := gitx.CaptureWIP(ctx, root, t.TempDir(), nil, "")
		if err != nil {
			t.Fatal(err)
		}
		payload := t.TempDir()
		if err = gitx.Bundle(ctx, root, filepath.Join(payload, "repo.bundle"), state.Commit, nil); err != nil {
			t.Fatal(err)
		}
		manifest := snapshot.Manifest{SchemaVersion: snapshot.SchemaVersion, ProjectID: "safety-project", SourceGitHead: state.Head, SourceBranch: state.Branch, Git: snapshot.Git{WIPCommit: state.Commit, IndexTree: state.IndexTree, WorktreeTree: state.WorktreeTree, UntrackedTree: state.UntrackedTree}}
		if _, err = Config(ctx, payload, manifest, root); err == nil || otgerror.Public(err).Code != otgerror.CodePackageInvalid {
			t.Fatalf("invalid incoming policy accepted: %v", err)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != policy {
			t.Fatalf("working policy changed: %q %v", data, err)
		}
	}
}

func TestDesiredEntriesAcceptsOnlyIdenticalCrossTreeDuplicates(t *testing.T) {
	root, _ := safetyRepo(t)
	tree := safetyTree(t, root, map[string]string{".onthego.yaml": "identical"})
	state := gitx.WIP{WorktreeTree: tree, UntrackedTree: tree}
	entries, err := desiredEntries(context.Background(), root, state)
	if err != nil || len(entries) != 1 {
		t.Fatalf("identical policy duplicate rejected: %#v %v", entries, err)
	}
	state.UntrackedTree = safetyTree(t, root, map[string]string{".onthego.yaml": "different"})
	if _, err = desiredEntries(context.Background(), root, state); err == nil {
		t.Fatal("conflicting cross-tree duplicate accepted")
	}
}
