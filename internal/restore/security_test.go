package restore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/packageotg"
	"github.com/peerproblem/onthego/internal/restore"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func TestRestoreRejectsSelectedPathsThroughGitSymlinksOrMetadata(t *testing.T) {
	for _, selected := range []string{"link/protected.txt", ".git/config"} {
		t.Run(selected, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("ONTHEGO_HOME", filepath.Join(base, "data"))
			repo, payload, outside := filepath.Join(base, "repo"), filepath.Join(base, "payload"), filepath.Join(base, "outside")
			for _, dir := range []string{repo, payload, outside} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			protected := filepath.Join(outside, "protected.txt")
			if err := os.WriteFile(protected, []byte("keep me"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				t.Helper()
				result, err := gitx.Run(context.Background(), repo, args...)
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(result.Stdout))
			}
			git("init", "-b", "main")
			git("add", "link")
			git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture")
			state, err := gitx.CaptureWIP(context.Background(), repo, base, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = gitx.Bundle(context.Background(), repo, filepath.Join(payload, "repo.bundle"), state.Commit, nil); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"index.patch", "worktree.patch"} {
				if err := os.WriteFile(filepath.Join(payload, name), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			archivePath := "files/" + selected
			if err := os.MkdirAll(filepath.Dir(filepath.Join(payload, archivePath)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(payload, archivePath), []byte("malicious"), 0o600); err != nil {
				t.Fatal(err)
			}
			manifest := snapshot.Manifest{SchemaVersion: snapshot.SchemaVersion, ProjectID: "test", SourceGitHead: git("rev-parse", "HEAD"), SourceBranch: "main", PayloadHashes: map[string]string{}}
			for _, name := range []string{"repo.bundle", "index.patch", "worktree.patch", archivePath} {
				data, err := os.ReadFile(filepath.Join(payload, name))
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(data)
				manifest.PayloadHashes[name] = hex.EncodeToString(hash[:])
			}
			manifest.Git.BundleSHA256 = manifest.PayloadHashes["repo.bundle"]
			manifest.Git.WIPCommit = state.Commit
			manifest.Git.IndexTree = state.IndexTree
			manifest.Git.WorktreeTree = state.WorktreeTree
			manifest.Git.UntrackedTree = state.UntrackedTree
			manifest.Files = []snapshot.File{{RelativePath: selected, ArchivePath: archivePath, Kind: "file", Mode: 0o600, SHA256: manifest.PayloadHashes[archivePath]}}
			keys, err := identity.LoadOrCreate()
			if err != nil {
				t.Fatal(err)
			}
			recipient, err := keys.Recipient()
			if err != nil {
				t.Fatal(err)
			}
			signer, err := keys.PrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(base, "snapshot.otg")
			if err := packageotg.Create(payload, archive, manifest, recipient, signer); err != nil {
				if otgerror.Public(err).Code != otgerror.CodePackageInvalid {
					t.Fatal(err)
				}
				if data, readErr := os.ReadFile(protected); readErr != nil || string(data) != "keep me" {
					t.Fatalf("outside file was modified: %q %v", data, readErr)
				}
				return
			}
			ageIdentity, err := keys.Identity()
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(base, "restored")
			if _, err := restore.Package(context.Background(), archive, destination, ageIdentity, keys.SigningPublic); err == nil {
				t.Fatal("unsafe selected path was restored")
			}
			if data, err := os.ReadFile(protected); err != nil || string(data) != "keep me" {
				t.Fatalf("outside file was modified: %q %v", data, err)
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("failed restore was published: %v", err)
			}
		})
	}
}
