package restore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func TestEncryptedBackupRejectsSymlinkParentWithoutDeletingRef(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("ONTHEGO_HOME", home)
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		r, e := gitx.Run(ctx, root, args...)
		if e != nil {
			t.Fatal(e)
		}
		return strings.TrimSpace(string(r.Stdout))
	}
	run("init", "-b", "main")
	run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	if e := os.WriteFile(filepath.Join(root, ".env"), []byte("never-in-git-secret"), 0600); e != nil {
		t.Fatal(e)
	}
	state, e := gitx.CaptureWIP(ctx, root, t.TempDir(), nil, "")
	if e != nil {
		t.Fatal(e)
	}
	b := backupState{WIP: state, Files: []snapshot.File{{RelativePath: ".env", Kind: "file", Secret: true, Mode: 0600}}, CreatedAt: time.Now().UTC()}
	if e = saveSecretBackup(ctx, root, &b); e != nil {
		t.Fatal(e)
	}
	body, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	commit, e := gitx.CommitTree(ctx, root, state.WorktreeTree, []string{state.Commit}, string(body))
	if e != nil {
		t.Fatal(e)
	}
	ref := backupPrefix(root) + "test"
	run("update-ref", ref, commit)
	external := filepath.Join(t.TempDir(), "external")
	if e = os.Rename(filepath.Join(home, "backups"), external); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(external, filepath.Join(home, "backups")); e != nil {
		t.Fatal(e)
	}
	if _, e = loadSecretBackup(ctx, b); e == nil {
		t.Fatal("decryption followed a symlink outside private backup directory")
	}
	if e = PruneBackups(ctx, root, 0); e == nil {
		t.Fatal("prune followed backup directory symlink")
	}
	if _, e = os.Stat(filepath.Join(external, b.SecretArchive)); e != nil {
		t.Fatalf("external encrypted backup was deleted: %v", e)
	}
	run("rev-parse", "--verify", ref)
}

func TestUndoPreservesAllDisplacedBranchTipsAndLinkedWorktrees(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "inverse preserves edited branch", true: "linked branch refuses undo"}[linked], func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("ONTHEGO_HOME", filepath.Join(t.TempDir(), "home"))
			root := t.TempDir()
			run := func(args ...string) string {
				t.Helper()
				r, e := gitx.Run(ctx, root, args...)
				if e != nil {
					t.Fatal(e)
				}
				return strings.TrimSpace(string(r.Stdout))
			}
			run("init", "-b", "main")
			run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
			head := run("rev-parse", "HEAD")
			tree := run("rev-parse", "HEAD^{tree}")
			commit := func(message string) string {
				t.Helper()
				id, e := gitx.CommitTree(ctx, root, tree, []string{head}, message)
				if e != nil {
					t.Fatal(e)
				}
				return id
			}
			oldTip, incomingTip, editedTip := commit("local feature before handoff"), commit("incoming feature"), commit("local feature after handoff")
			run("update-ref", "refs/heads/feature", oldTip)
			ref, e := createBackup(ctx, root, nil, map[string]gitx.TreeEntry{}, nil, "feature")
			if e != nil {
				t.Fatal(e)
			}
			run("update-ref", "refs/heads/feature", incomingTip)
			if linked {
				path := filepath.Join(t.TempDir(), "linked")
				run("worktree", "add", path, "feature")
				if _, e = Undo(ctx, root, ref); e == nil {
					t.Fatal("undo rewrote branch checked out by linked worktree")
				}
				if got := run("rev-parse", "refs/heads/feature"); got != incomingTip {
					t.Fatal("ref changed during refused undo")
				}
				run("rev-parse", ref)
				return
			}
			run("update-ref", "refs/heads/feature", editedTip)
			undone, e := Undo(ctx, root, ref)
			if e != nil {
				t.Fatal(e)
			}
			if got := run("rev-parse", "refs/heads/feature"); got != oldTip {
				t.Fatal("undo lost displaced original branch")
			}
			if _, e = Undo(ctx, root, undone.Backup); e != nil {
				t.Fatal(e)
			}
			if got := run("rev-parse", "refs/heads/feature"); got != editedTip {
				t.Fatal("undo inverse lost branch edited after handoff")
			}
		})
	}
}
