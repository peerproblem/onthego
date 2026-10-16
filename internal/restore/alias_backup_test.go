package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func TestAliasedRenameApplyAndUndo(t *testing.T) {
	for _, pair := range [][2]string{{"Foo.txt", "foo.txt"}, {"caf\u00e9.txt", "cafe\u0301.txt"}} {
		for _, body := range []string{"original", "changed"} {
			for _, tracking := range []string{"untracked", "tracked", "staged deletion"} {
				t.Run(pair[0]+"/"+body+"/"+tracking, func(t *testing.T) {
					ctx := context.Background()
					root, run := safetyRepo(t)
					key, err := pathKeyer(root)
					if err != nil {
						t.Fatal(err)
					}
					if key(pair[0]) != key(pair[1]) {
						t.Skip("filesystem distinguishes fixture names")
					}
					if err = os.WriteFile(filepath.Join(root, pair[0]), []byte("original"), 0600); err != nil {
						t.Fatal(err)
					}
					if tracking != "untracked" {
						run("add", pair[0])
						run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "tracked file")
						if tracking == "staged deletion" {
							run("rm", "--cached", pair[0])
						}
					}
					head, originalIndex := run("rev-parse", "HEAD"), run("write-tree")
					empty := safetyTree(t, root, nil)
					tree := safetyTree(t, root, map[string]string{pair[1]: body})
					indexTree, worktreeTree, extrasTree := empty, empty, tree
					var files []snapshot.File
					var previous []string
					if tracking == "tracked" {
						indexTree, worktreeTree, extrasTree = tree, tree, empty
					} else {
						hash := sha256.Sum256([]byte(body))
						files = []snapshot.File{{RelativePath: pair[1], Kind: "file", Mode: 0600, SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}}
						previous = []string{pair[0]}
					}
					index, err := gitx.CommitTree(ctx, root, indexTree, []string{head}, "index")
					if err != nil {
						t.Fatal(err)
					}
					extras, err := gitx.CommitTree(ctx, root, extrasTree, nil, "extras")
					if err != nil {
						t.Fatal(err)
					}
					commit, err := gitx.CommitTree(ctx, root, worktreeTree, []string{head, index, extras}, "rename fixture")
					if err != nil {
						t.Fatal(err)
					}
					payload := t.TempDir()
					if err = gitx.Bundle(ctx, root, filepath.Join(payload, "repo.bundle"), commit, nil); err != nil {
						t.Fatal(err)
					}
					manifest := snapshot.Manifest{SchemaVersion: snapshot.SchemaVersion, SourceGitHead: head, SourceBranch: "main", Files: files, Git: snapshot.Git{WIPCommit: commit, IndexTree: indexTree, WorktreeTree: worktreeTree, UntrackedTree: extrasTree}}
					applied, err := Apply(ctx, payload, root, manifest, previous)
					if err != nil {
						t.Fatal(err)
					}
					assertAliasFile(t, root, pair[1], pair[0], body)
					if _, err = Undo(ctx, root, applied.Backup); err != nil {
						t.Fatalf("undo renamed file: %v", err)
					}
					assertAliasFile(t, root, pair[0], pair[1], "original")
					if run("rev-parse", "HEAD") != head || run("write-tree") != originalIndex {
						t.Fatal("undo changed original HEAD or staged state")
					}
				})
			}
		}
	}
}

func assertAliasFile(t *testing.T, root, want, old, body string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, want))
	if err != nil || string(data) != body {
		t.Fatalf("%s content = %q, error = %v", want, data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == old {
			t.Fatalf("old spelling %q remains", old)
		}
		found = found || entry.Name() == want
	}
	if !found {
		t.Fatalf("desired spelling %q is absent", want)
	}
}

func TestBackupSecretAliasesStayOutsideGit(t *testing.T) {
	ctx := context.Background()
	root, _ := safetyRepo(t)
	key, err := pathKeyer(root)
	if err != nil {
		t.Fatal(err)
	}
	if key(".NPMRC") != key(".npmrc") {
		t.Skip("case sensitive filesystem")
	}
	const token = "alias-secret-must-never-become-a-git-object"
	if err = os.WriteFile(filepath.Join(root, ".NPMRC"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := createBackup(ctx, root, []string{".NPMRC"}, map[string]gitx.TreeEntry{".npmrc": {Path: ".npmrc"}}, []string{".npmrc"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := readBackup(ctx, root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Files) != 1 || saved.Files[0].RelativePath != ".NPMRC" || !saved.Files[0].Secret || saved.SecretArchive == "" {
		t.Fatalf("secret alias backup = %#v", saved)
	}
	entries, err := gitx.Tree(ctx, root, saved.WIP.UntrackedTree)
	if err != nil || len(entries) != 0 {
		t.Fatalf("secret entered untracked tree: %#v, %v", entries, err)
	}
	hash, err := gitx.RunInput(ctx, root, nil, []byte(token), "hash-object", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = gitx.Run(ctx, root, "cat-file", "-e", strings.TrimSpace(string(hash.Stdout))); err == nil {
		t.Fatal("secret alias was written as a Git blob")
	}
	if err = os.Rename(filepath.Join(root, ".NPMRC"), filepath.Join(root, ".npmrc")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".npmrc"), []byte("new-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Undo(ctx, root, ref); err != nil {
		t.Fatal(err)
	}
	assertAliasFile(t, root, ".NPMRC", ".npmrc", token)
}

func TestBackupUsesExistingSupplementalSpelling(t *testing.T) {
	ctx := context.Background()
	root, _ := safetyRepo(t)
	key, err := pathKeyer(root)
	if err != nil {
		t.Fatal(err)
	}
	if key("Foo") != key("foo") {
		t.Skip("case sensitive filesystem")
	}
	if err = os.Mkdir(filepath.Join(root, "DIR"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "DIR", "Foo"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := createBackup(ctx, root, []string{"dir/foo"}, nil, nil, "main")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := readBackup(ctx, root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Files) != 1 || saved.Files[0].RelativePath != "DIR/Foo" {
		t.Fatalf("backup did not preserve actual spelling: %#v", saved.Files)
	}
}
