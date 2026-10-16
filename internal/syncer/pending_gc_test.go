package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/packageotg"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func TestPendingExportsSurviveGCAndOldestReturn(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	ctx := context.Background()
	var exports []snapshot.Manifest
	for i := 0; i < 3; i++ {
		write(t, filepath.Join(p.aRoot, "file.txt"), fmt.Sprintf("pending export %d\n", i))
		manifest, err := Export(ctx, p.aRoot, filepath.Join(p.dir, fmt.Sprintf("pending-%d.otg", i)), p.b.keys.AgeRecipient, "test", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		exports = append(exports, manifest)
	}
	// The first export is still the user's intended outbound state. The two
	// newer offline packages have never been delivered or acknowledged.
	write(t, filepath.Join(p.aRoot, "file.txt"), "pending export 0\n")
	git(t, p.aRoot, "reflog", "expire", "--expire=now", "--all")
	git(t, p.aRoot, "gc", "--prune=now")
	p.b.use(t)
	if _, err := Apply(ctx, filepath.Join(p.dir, "pending-0.otg"), p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.bRoot, "file.txt"), "work returned from oldest export\n")
	returned := filepath.Join(p.dir, "pending-return.otg")
	if _, err := Export(ctx, p.bRoot, returned, p.a.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.a.use(t)
	if _, err := Apply(ctx, returned, p.aRoot, p.b.keys.SigningPublic, false); err != nil {
		t.Fatalf("oldest pending return after GC requires replacement: %v", err)
	}
	if got := read(t, filepath.Join(p.aRoot, "file.txt")); got != "work returned from oldest export\n" {
		t.Fatalf("return not applied: %q", got)
	}
	for _, exported := range exports {
		if got := git(t, p.aRoot, "rev-parse", "refs/onthego/pending/"+exported.SnapshotID); got != exported.Git.WIPCommit {
			t.Fatalf("pending export is not pinned: %s", exported.SnapshotID)
		}
	}
	// A later bundle is built with earlier pending refs already present.
	p.b.use(t)
	key, err := p.b.keys.Identity()
	if err != nil {
		t.Fatal(err)
	}
	unpacked := filepath.Join(p.dir, "bundle-inspection")
	if _, err := packageotg.Extract(filepath.Join(p.dir, "pending-2.otg"), unpacked, key, p.a.keys.SigningPublic); err != nil {
		t.Fatal(err)
	}
	heads := git(t, p.bRoot, "bundle", "list-heads", filepath.Join(unpacked, "repo.bundle"))
	if strings.Contains(heads, "refs/onthego/pending/") {
		t.Fatalf("local pending refs leaked into bundle: %s", heads)
	}
}

func TestAcceptedBaselineSkipsMissingLegacyPendingCommit(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	available := baseline{SchemaVersion: 2, Root: p.aRoot, ProjectID: cfg.ProjectID, SnapshotID: strings.Repeat("1", 64), WIPCommit: git(t, p.aRoot, "rev-parse", "HEAD"), Fingerprint: strings.Repeat("a", 64), UpdatedAt: now.Add(-time.Minute)}
	missing := available
	missing.SnapshotID, missing.WIPCommit, missing.UpdatedAt = strings.Repeat("2", 64), strings.Repeat("f", 40), now
	for _, pending := range []baseline{available, missing} {
		path, err := pendingPath(p.aRoot, cfg.ProjectID, pending.SnapshotID)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(path, pending); err != nil {
			t.Fatal(err)
		}
	}
	accepted, _, err := acceptedBaseline(context.Background(), p.aRoot, cfg.ProjectID, baseline{}, available.Fingerprint, snapshot.Manifest{Git: snapshot.Git{WIPCommit: available.WIPCommit}})
	if err != nil || accepted.SnapshotID != available.SnapshotID {
		t.Fatalf("missing legacy candidate prevented available candidate: %+v %v", accepted, err)
	}
}

func TestPendingRefCleanupFollowsReceipts(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	commit := git(t, p.aRoot, "rev-parse", "HEAD")
	for i := 0; i < 66; i++ {
		pending := baseline{SchemaVersion: 2, Root: p.aRoot, ProjectID: cfg.ProjectID, SnapshotID: fmt.Sprintf("%064x", i+1), WIPCommit: commit, Fingerprint: strings.Repeat("a", 64), UpdatedAt: now.Add(-time.Duration(i) * time.Minute)}
		if i == 65 {
			pending.UpdatedAt = now.Add(-pendingTTL - time.Hour)
		}
		path, err := pendingPath(p.aRoot, cfg.ProjectID, pending.SnapshotID)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(path, pending); err != nil {
			t.Fatal(err)
		}
		git(t, p.aRoot, "update-ref", "refs/onthego/pending/"+pending.SnapshotID, commit)
	}
	orphan := strings.Repeat("e", 64)
	git(t, p.aRoot, "update-ref", "refs/onthego/pending/"+orphan, commit)
	retained, err := pendingBaselines(p.aRoot, cfg.ProjectID)
	if err != nil || len(retained) != 64 {
		t.Fatalf("pending cleanup: %d %v", len(retained), err)
	}
	refs := strings.Fields(git(t, p.aRoot, "for-each-ref", "--format=%(refname)", "refs/onthego/pending"))
	if len(refs) != 64 {
		t.Fatalf("TTL, overflow or orphan pin remains: %d refs", len(refs))
	}
	for _, pending := range retained {
		if got := git(t, p.aRoot, "rev-parse", "refs/onthego/pending/"+pending.SnapshotID); got != commit {
			t.Fatalf("retained pin changed: %s", pending.SnapshotID)
		}
	}
	id := retained[0].SnapshotID
	if err := MarkSynced(context.Background(), p.aRoot, id); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(context.Background(), p.aRoot, "show-ref", "--verify", "refs/onthego/pending/"+id); err == nil {
		t.Fatal("acknowledgment retained pending pin")
	}
	path, err := pendingPath(p.aRoot, cfg.ProjectID, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("acknowledgment retained receipt: %v", err)
	}
	olderID := retained[1].SnapshotID
	if err := MarkSyncedPeer(context.Background(), p.aRoot, olderID, "other-peer"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(context.Background(), p.aRoot, "show-ref", "--verify", "refs/onthego/pending/"+olderID); err == nil {
		t.Fatal("out-of-order acknowledgment retained pending pin")
	}
}

func TestPendingRefCleanupPreservesOtherWorktreeReceipt(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	otherRoot := filepath.Join(p.dir, "other-worktree")
	git(t, p.aRoot, "worktree", "add", "-b", "other", otherRoot)
	otherConfig := config.Default()
	if err := config.Save(otherRoot, otherConfig); err != nil {
		t.Fatal(err)
	}
	pending := baseline{SchemaVersion: 2, Root: otherRoot, ProjectID: otherConfig.ProjectID, SnapshotID: strings.Repeat("c", 64), WIPCommit: git(t, otherRoot, "rev-parse", "HEAD"), Fingerprint: strings.Repeat("a", 64), UpdatedAt: time.Now().UTC()}
	path, err := pendingPath(otherRoot, otherConfig.ProjectID, pending.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, pending); err != nil {
		t.Fatal(err)
	}
	git(t, otherRoot, "update-ref", "refs/onthego/pending/"+pending.SnapshotID, pending.WIPCommit)
	// Run cleanup from a root with no pending directory of its own.
	orphan := strings.Repeat("d", 64)
	git(t, p.aRoot, "update-ref", "refs/onthego/pending/"+orphan, pending.WIPCommit)
	if _, err := pendingBaselines(p.aRoot, cfg.ProjectID); err != nil {
		t.Fatal(err)
	}
	if got := git(t, otherRoot, "rev-parse", "refs/onthego/pending/"+pending.SnapshotID); got != pending.WIPCommit {
		t.Fatal("cleanup removed another worktree's pin")
	}
	if _, err := gitx.Run(context.Background(), p.aRoot, "show-ref", "--verify", "refs/onthego/pending/"+orphan); err == nil {
		t.Fatal("orphan ref remains when local pending directory is absent")
	}
}

func TestPendingCommitLookupDoesNotHideGitFailures(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	ctx := context.Background()
	missing := strings.Repeat("f", 40)
	if exists, err := pendingCommitExists(ctx, p.aRoot, missing); err != nil || exists {
		t.Fatalf("missing object: %v %v", exists, err)
	}
	if exists, err := pendingCommitExists(ctx, p.dir, missing); err == nil || exists {
		t.Fatalf("repository failure was treated as a missing object: %v %v", exists, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := pendingCommitExists(canceled, p.aRoot, missing); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was treated as a missing object: %v", err)
	}
}
