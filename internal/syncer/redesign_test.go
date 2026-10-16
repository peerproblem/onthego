package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func assertNoPlaintextStaging(t *testing.T, m machine) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.state, "tmp"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != ".leases" {
			t.Fatalf("plaintext temporary directory remains: %s", entry.Name())
		}
	}
}

func assertSecretNotStoredInGit(t *testing.T, root, secret string) {
	t.Helper()
	calculated, err := gitx.RunInput(context.Background(), root, nil, []byte(secret), "hash-object", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.TrimSpace(string(calculated.Stdout))
	if _, err := gitx.Run(context.Background(), root, "cat-file", "-e", oid); err == nil {
		t.Fatal("explicitly selected secret was retained as a plaintext Git object")
	}
}

func sessionRecoveryDirectory(t *testing.T, m machine) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(m.state, "session-recovery-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one private session recovery directory, got %v: %v", matches, err)
	}
	info, err := os.Stat(matches[0])
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("session recovery directory is not private: %v %v", info, err)
	}
	return matches[0]
}

func TestExactSnapshotReplayPreservesPostHandoffEdits(t *testing.T) {
	p := paired(t)
	id := p.firstHandoff(t)
	p.b.use(t)
	write(t, filepath.Join(p.bRoot, "file.txt"), "edited after delivery\n")
	git(t, p.bRoot, "add", "file.txt")
	write(t, filepath.Join(p.bRoot, "file.txt"), "more local work\n")
	indexBefore := git(t, p.bRoot, "write-tree")
	backupsBefore := git(t, p.bRoot, "for-each-ref", "--format=%(refname) %(objectname)", "refs/onthego/backup")
	for _, replace := range []bool{false, true} {
		result, err := Apply(context.Background(), filepath.Join(p.dir, "first.otg"), p.bRoot, p.a.keys.SigningPublic, replace)
		if err != nil || !result.AlreadyApplied || result.Manifest.SnapshotID != id || result.Backup != "" {
			t.Fatalf("replay: %+v %v", result, err)
		}
		if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "more local work\n" {
			t.Fatalf("replay discarded edit: %q", got)
		}
		if got := git(t, p.bRoot, "write-tree"); got != indexBefore {
			t.Fatal("replay replaced the index")
		}
	}
	if got := git(t, p.bRoot, "for-each-ref", "--format=%(refname) %(objectname)", "refs/onthego/backup"); got != backupsBefore {
		t.Fatal("replay created backup objects")
	}
}

func TestLostResponseReconciliationAllowsSubsequentHandoff(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "file.txt"), "delivered without acknowledgment\n")
	path := filepath.Join(p.dir, "response-lost.otg")
	manifest, err := ExportWithOptions(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", ExportOptions{Peer: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	remote, err := Status(context.Background(), p.bRoot)
	if err != nil {
		t.Fatal(err)
	}
	p.a.use(t)
	// A different authenticated peer cannot claim this peer's pending receipt.
	if err := ReconcilePending(context.Background(), p.aRoot, remote.SnapshotID, "unrelated"); err != nil {
		t.Fatal(err)
	}
	before, err := Status(context.Background(), p.aRoot)
	if err != nil || before.SnapshotID == manifest.SnapshotID {
		t.Fatalf("wrong-peer receipt acknowledged: %+v %v", before, err)
	}
	if err := ReconcilePending(context.Background(), p.aRoot, remote.SnapshotID, "laptop"); err != nil {
		t.Fatal(err)
	}
	after, err := Status(context.Background(), p.aRoot)
	if err != nil || after.SnapshotID != manifest.SnapshotID || after.Changed {
		t.Fatalf("lost response was not reconciled: %+v %v", after, err)
	}
	write(t, filepath.Join(p.aRoot, "file.txt"), "follow-up after reconciliation\n")
	transfer(t, p.dir, "reconciled-followup", p.a, p.aRoot, p.b, p.bRoot)
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "follow-up after reconciliation\n" {
		t.Fatal(got)
	}
}

func TestOfflinePendingRoundTripRequiresNoReplacement(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "file.txt"), "offline outbound\n")
	outbound, err := Export(context.Background(), p.aRoot, filepath.Join(p.dir, "offline-out.otg"), p.b.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), filepath.Join(p.dir, "offline-out.otg"), p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.bRoot, "file.txt"), "offline return\n")
	returned, err := Export(context.Background(), p.bRoot, filepath.Join(p.dir, "offline-back.otg"), p.a.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.a.use(t)
	status, err := Status(context.Background(), p.aRoot)
	if err != nil || status.HasBaseline {
		t.Fatalf("offline pack must remain pending before return: %+v %v", status, err)
	}
	if _, err := Apply(context.Background(), filepath.Join(p.dir, "offline-back.otg"), p.aRoot, p.b.keys.SigningPublic, false); err != nil {
		t.Fatalf("offline return requires --replace: %v", err)
	}
	if got := read(t, filepath.Join(p.aRoot, "file.txt")); got != "offline return\n" {
		t.Fatal(got)
	}
	status, err = Status(context.Background(), p.aRoot)
	if err != nil || status.SnapshotID != returned.SnapshotID || status.Changed {
		t.Fatalf("returned baseline: %+v %v", status, err)
	}
	if ok, err := gitx.IsAncestor(context.Background(), p.aRoot, outbound.Git.WIPCommit, returned.Git.WIPCommit); err != nil || !ok {
		t.Fatalf("offline WIP ancestry missing: %v %v", ok, err)
	}
}

func TestPeerReceiptsAndActiveDeviceWarningAreIndependent(t *testing.T) {
	p := paired(t)
	c := newMachine(t, filepath.Join(p.dir, "c"))
	cRoot := filepath.Join(p.dir, "c", "project")
	ids := map[string]string{}
	for _, dest := range []struct {
		name string
		m    machine
		root string
	}{{"laptop", p.b, p.bRoot}, {"desktop", c, cRoot}} {
		p.a.use(t)
		path := filepath.Join(p.dir, "peer-"+dest.name+".otg")
		manifest, err := ExportWithOptions(context.Background(), p.aRoot, path, dest.m.keys.AgeRecipient, "test", ExportOptions{Peer: dest.name})
		if err != nil {
			t.Fatal(err)
		}
		dest.m.use(t)
		if _, err := ApplyWithOptions(context.Background(), path, dest.root, p.a.keys.SigningPublic, ApplyOptions{Peer: "origin"}); err != nil {
			t.Fatal(err)
		}
		destination, err := Status(context.Background(), dest.root)
		if err != nil || destination.ActiveDevice != "" {
			t.Fatalf("receiver should become active locally: %+v %v", destination, err)
		}
		p.a.use(t)
		if err := MarkSyncedPeer(context.Background(), p.aRoot, manifest.SnapshotID, dest.name); err != nil {
			t.Fatal(err)
		}
		ids[dest.name] = manifest.SnapshotID
	}
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	for peer, id := range ids {
		path, err := peerStatePath(p.aRoot, cfg.ProjectID, peer)
		if err != nil {
			t.Fatal(err)
		}
		var receipt baseline
		if err := readJSON(path, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Peer != peer || receipt.SnapshotID != id {
			t.Fatalf("peer receipt lost: %+v", receipt)
		}
	}
	status, err := Status(context.Background(), p.aRoot)
	if err != nil || status.ActiveDevice != "desktop" || status.HandedOffAt.IsZero() {
		t.Fatalf("active device: %+v %v", status, err)
	}
	if warning := strings.Join(status.Warnings, "\n"); !strings.Contains(warning, "desktop") || !strings.Contains(strings.ToLower(warning), "session") {
		t.Fatalf("missing workspace/session warning: %q", warning)
	}
}

func TestPendingReceiptsExpireAndRetainOnlyNewest64(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	paths := map[int]string{}
	for i := 0; i < 70; i++ {
		created := now.Add(-time.Duration(i) * time.Minute)
		if i == 69 {
			created = now.Add(-pendingTTL - time.Hour)
		}
		pending := baseline{SchemaVersion: 2, Root: p.aRoot, ProjectID: cfg.ProjectID, SnapshotID: fmt.Sprintf("%064x", i+1), Fingerprint: strings.Repeat("a", 64), UpdatedAt: created}
		path, err := pendingPath(p.aRoot, cfg.ProjectID, pending.SnapshotID)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(path, pending); err != nil {
			t.Fatal(err)
		}
		paths[i] = path
	}
	retained, err := pendingBaselines(p.aRoot, cfg.ProjectID)
	if err != nil || len(retained) != 64 {
		t.Fatalf("pending retention: %d %v", len(retained), err)
	}
	for i := 0; i < 70; i++ {
		_, err := os.Stat(paths[i])
		if i < 64 && err != nil {
			t.Fatalf("recent receipt %d pruned: %v", i, err)
		}
		if i >= 64 && !os.IsNotExist(err) {
			t.Fatalf("expired/overflow receipt %d retained: %v", i, err)
		}
	}
}

func TestInPlaceApplyPreservesGitConfigurationHooksAndLocalInodes(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.b.use(t)
	git(t, p.bRoot, "config", "user.name", "Receiver Identity")
	git(t, p.bRoot, "config", "user.email", "receiver@example.test")
	git(t, p.bRoot, "config", "core.hooksPath", ".husky")
	git(t, p.bRoot, "config", "rerere.enabled", "true")
	git(t, p.bRoot, "remote", "add", "receiver-only", "https://example.test/receiver/repo.git")
	git(t, p.bRoot, "update-ref", "refs/remotes/receiver-only/main", "HEAD")
	git(t, p.bRoot, "config", "branch.main.remote", "receiver-only")
	git(t, p.bRoot, "config", "branch.main.merge", "refs/heads/main")
	write(t, filepath.Join(p.bRoot, "file.txt"), "local stash\n")
	git(t, p.bRoot, "stash", "push", "-m", "receiver stash")
	stash := git(t, p.bRoot, "rev-parse", "refs/stash")
	upstream := git(t, p.bRoot, "rev-parse", "@{u}")
	write(t, filepath.Join(p.bRoot, ".git", "info", "exclude"), "node_modules/\n.husky/\n")
	write(t, filepath.Join(p.bRoot, ".husky", "pre-commit"), "#!/bin/sh\nexit 1\n")
	write(t, filepath.Join(p.bRoot, ".env"), "RECEIVER_ONLY=keep\n")
	write(t, filepath.Join(p.bRoot, "node_modules", "module", "index.js"), "receiver dependency\n")
	paths := []string{p.bRoot, filepath.Join(p.bRoot, ".git"), filepath.Join(p.bRoot, ".git", "config"), filepath.Join(p.bRoot, ".husky", "pre-commit"), filepath.Join(p.bRoot, ".env"), filepath.Join(p.bRoot, "node_modules"), filepath.Join(p.bRoot, "node_modules", "module", "index.js")}
	before := map[string]os.FileInfo{}
	contents := map[string]string{}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = info
		if info.Mode().IsRegular() {
			contents[path] = read(t, path)
		}
	}
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "file.txt"), "incoming changed file\n")
	transfer(t, p.dir, "in-place", p.a, p.aRoot, p.b, p.bRoot)
	for _, path := range paths {
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before[path], after) {
			t.Fatalf("local inode replaced: %s", path)
		}
		if want, ok := contents[path]; ok && read(t, path) != want {
			t.Fatalf("local contents replaced: %s", path)
		}
	}
	if got := git(t, p.bRoot, "rev-parse", "refs/stash"); got != stash {
		t.Fatal("receiver stash replaced")
	}
	if got := git(t, p.bRoot, "rev-parse", "@{u}"); got != upstream {
		t.Fatal("receiver upstream replaced")
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "incoming changed file\n" {
		t.Fatal(got)
	}
	legacy, err := filepath.Glob(filepath.Join(filepath.Dir(p.bRoot), ".onthego-backup-*"))
	if err != nil || len(legacy) != 0 {
		t.Fatalf("directory backup remains: %v %v", legacy, err)
	}
}

func TestSessionDivergenceDoesNotBlockAndLeavesNoWorkspaceTranscript(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	source := codexSession(t, p.a, p.aRoot, "diverged", "shared history")
	p.firstHandoff(t)
	target := filepath.Join(p.b.codex, "sessions", "2026", "10", "16", filepath.Base(source))
	write(t, source, read(t, source)+"{\"type\":\"response_item\",\"payload\":{\"text\":\"sender branch\"}}\n")
	write(t, target, read(t, target)+"{\"type\":\"response_item\",\"payload\":{\"text\":\"receiver branch\"}}\n")
	originalTarget := read(t, target)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "file.txt"), "independent workspace update\n")
	path := filepath.Join(p.dir, "diverged.otg")
	if _, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	result, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions.Conflicts != 1 || len(result.Sessions.PreservedPaths) != 1 {
		t.Fatalf("missing session conflict recovery: %+v", result.Sessions)
	}
	if read(t, target) != originalTarget {
		t.Fatal("destination session was overwritten")
	}
	archive := result.Sessions.PreservedPaths[0]
	if !strings.HasPrefix(archive, p.b.state+string(filepath.Separator)) || !strings.Contains(read(t, archive), "sender branch") {
		t.Fatalf("private incoming history missing: %s", archive)
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "independent workspace update\n" {
		t.Fatal(got)
	}
	for _, root := range []string{p.aRoot, p.bRoot} {
		if _, err := os.Stat(filepath.Join(root, ".onthego.local", "context", "native-sessions")); !os.IsNotExist(err) {
			t.Fatalf("workspace plaintext sessions retained: %s %v", root, err)
		}
	}
	assertNoPlaintextStaging(t, p.a)
	assertNoPlaintextStaging(t, p.b)
}

func TestSameAgentHomeHandoffPreservesOriginalSession(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	path := codexSession(t, p.a, p.aRoot, "same-home", "original workspace history")
	original := read(t, path)
	archive := filepath.Join(p.dir, "same-home.otg")
	if _, err := Export(context.Background(), p.aRoot, archive, p.a.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil || result.Sessions.Skipped != 1 || len(result.Sessions.Warnings) == 0 {
		t.Fatalf("same agent home handoff: %+v %v", result, err)
	}
	if read(t, path) != original {
		t.Fatal("same-home import rewrote original workspace session")
	}
}

func TestThreeMachineWIPAncestryRemainsBoundedInManifest(t *testing.T) {
	p := paired(t)
	c := newMachine(t, filepath.Join(p.dir, "c"))
	machines := []machine{p.a, p.b, c}
	roots := []string{p.aRoot, p.bRoot, filepath.Join(p.dir, "c", "project")}
	var previous snapshot.Manifest
	for step := 0; step < 9; step++ {
		from, to := step%3, (step+1)%3
		machines[from].use(t)
		write(t, filepath.Join(roots[from], "file.txt"), fmt.Sprintf("rotation %d\n", step))
		archive := filepath.Join(p.dir, fmt.Sprintf("rotation-%d.otg", step))
		manifest, err := Export(context.Background(), roots[from], archive, machines[to].keys.AgeRecipient, "test", nil, nil)
		if err != nil {
			t.Fatalf("export rotation %d: %v", step, err)
		}
		if len(manifest.ParentSnapshotIDs) > 1 {
			t.Fatalf("snapshot ancestry grew with handoffs: %v", manifest.ParentSnapshotIDs)
		}
		parents := strings.Fields(git(t, roots[from], "show", "-s", "--format=%P", manifest.Git.WIPCommit))
		if len(parents) > 4 || len(parents) < 3 {
			t.Fatalf("WIP parent count: %v", parents)
		}
		if step > 0 {
			if ok, err := gitx.IsAncestor(context.Background(), roots[from], previous.Git.WIPCommit, manifest.Git.WIPCommit); err != nil || !ok {
				t.Fatalf("handoff ancestry lost at rotation %d: %v %v", step, ok, err)
			}
		}
		machines[to].use(t)
		if _, err := Apply(context.Background(), archive, roots[to], machines[from].keys.SigningPublic, false); err != nil {
			t.Fatalf("apply rotation %d: %v", step, err)
		}
		if got := read(t, filepath.Join(roots[to], "file.txt")); got != fmt.Sprintf("rotation %d\n", step) {
			t.Fatal(got)
		}
		machines[from].use(t)
		if err := MarkSynced(context.Background(), roots[from], manifest.SnapshotID); err != nil {
			t.Fatal(err)
		}
		previous = manifest
	}
}

func TestPartialFinalSessionLineStillDeliversWorkspaceAndValidPrefix(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	path := codexSession(t, p.a, p.aRoot, "truncated", "complete line")
	prefix := read(t, path)
	write(t, path, prefix+`{"type":"response_item","payload":{"text":"interrupted`)
	archive := filepath.Join(p.dir, "truncated.otg")
	if _, err := ExportWithOptions(context.Background(), p.aRoot, archive, p.b.keys.AgeRecipient, "test", ExportOptions{Selection: agentsessions.Selection{All: true}}); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	result, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil || result.Sessions.Imported != 1 {
		t.Fatalf("truncated session blocked workspace: %+v %v", result, err)
	}
	if warning := strings.ToLower(strings.Join(result.Sessions.Warnings, "\n")); !strings.Contains(warning, "incomplete") {
		t.Fatalf("truncated session tail was omitted silently: %q", warning)
	}
	want := strings.ReplaceAll(prefix, p.aRoot, p.bRoot)
	if got := read(t, filepath.Join(p.b.codex, "sessions", "2026", "10", "16", filepath.Base(path))); got != want {
		t.Fatalf("valid session prefix changed: %q", got)
	}
}

func TestDelayedPeerAcknowledgmentDoesNotRewindCurrentBaseline(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	older, err := ExportWithOptions(context.Background(), p.aRoot, filepath.Join(p.dir, "older.otg"), p.b.keys.AgeRecipient, "test", ExportOptions{Peer: "slow-peer"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.aRoot, "file.txt"), "newer handoff\n")
	newer, err := ExportWithOptions(context.Background(), p.aRoot, filepath.Join(p.dir, "newer.otg"), p.b.keys.AgeRecipient, "test", ExportOptions{Peer: "fast-peer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkSyncedPeer(context.Background(), p.aRoot, newer.SnapshotID, "fast-peer"); err != nil {
		t.Fatal(err)
	}
	if err := MarkSyncedPeer(context.Background(), p.aRoot, older.SnapshotID, "slow-peer"); err != nil {
		t.Fatal(err)
	}
	status, err := Status(context.Background(), p.aRoot)
	if err != nil || status.SnapshotID != newer.SnapshotID || status.ActiveDevice != "fast-peer" || status.Changed {
		t.Fatalf("delayed receipt rewound current workspace: %+v %v", status, err)
	}
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	for peer, want := range map[string]string{"slow-peer": older.SnapshotID, "fast-peer": newer.SnapshotID} {
		path, err := peerStatePath(p.aRoot, cfg.ProjectID, peer)
		if err != nil {
			t.Fatal(err)
		}
		var receipt baseline
		if err := readJSON(path, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.SnapshotID != want {
			t.Fatalf("receipt lost for %s: %+v", peer, receipt)
		}
	}
}

func TestSessionImportIOFailureStillCommitsWorkspaceAndPreservesLogs(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	source := codexSession(t, p.a, p.aRoot, "io-failure", "recover despite disk failure")
	archive := filepath.Join(p.dir, "import-io.otg")
	manifest, err := Export(context.Background(), p.aRoot, archive, p.b.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	// A directory at the lock pathname produces a real open error on all UIDs,
	// unlike a chmod-based fixture which silently passes when CI runs as root.
	if err := os.MkdirAll(filepath.Join(p.b.codex, ".onthego-import.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil {
		t.Fatalf("adapter I/O failure blocked workspace: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("adapter I/O failure was silent")
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "committed\n" {
		t.Fatal(got)
	}
	recovered := filepath.Join(sessionRecoveryDirectory(t, p.b), "codex", "sessions", "2026", "10", "16", filepath.Base(source))
	if got := read(t, recovered); got != read(t, source) {
		t.Fatal("adapter I/O error discarded recoverable transcript")
	}
	status, err := Status(context.Background(), p.bRoot)
	if err != nil || status.SnapshotID != manifest.SnapshotID || status.Changed {
		t.Fatalf("delivered workspace receipt missing: %+v %v", status, err)
	}
	assertNoPlaintextStaging(t, p.b)
}

func TestAdapterDestinationSecurityErrorsPreserveSessionsWithoutBlockingWorkspace(t *testing.T) {
	for _, obstruction := range []string{"public-conflict-directory", "symlink-session", "symlink-session-directory"} {
		t.Run(obstruction, func(t *testing.T) {
			p := paired(t)
			p.a.use(t)
			source := codexSession(t, p.a, p.aRoot, "adapter-security", "shared history")
			p.firstHandoff(t)
			target := filepath.Join(p.b.codex, "sessions", "2026", "10", "16", filepath.Base(source))
			write(t, source, read(t, source)+"{\"type\":\"response_item\",\"payload\":{\"text\":\"incoming continuation\"}}\n")
			write(t, target, read(t, target)+"{\"type\":\"response_item\",\"payload\":{\"text\":\"local continuation\"}}\n")
			originalTarget := read(t, target)
			outside := filepath.Join(p.dir, "outside-session.jsonl")
			if obstruction == "public-conflict-directory" {
				conflicts := filepath.Join(p.b.state, "conflicts")
				if err := os.MkdirAll(conflicts, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(conflicts, 0755); err != nil {
					t.Fatal(err)
				}
			} else if obstruction == "symlink-session-directory" {
				outside = filepath.Join(p.dir, "outside-sessions")
				if err := os.Rename(filepath.Join(p.b.codex, "sessions"), outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(p.b.codex, "sessions")); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, outside, originalTarget)
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			}
			p.a.use(t)
			write(t, filepath.Join(p.aRoot, "file.txt"), "workspace survives adapter rejection\n")
			archive := filepath.Join(p.dir, "adapter-security.otg")
			manifest, err := Export(context.Background(), p.aRoot, archive, p.b.keys.AgeRecipient, "test", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			p.b.use(t)
			result, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false)
			if err != nil {
				t.Fatalf("adapter security error blocked independent workspace: %v", err)
			}
			if len(result.Warnings) == 0 {
				t.Fatal("adapter security error was silent")
			}
			if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "workspace survives adapter rejection\n" {
				t.Fatal(got)
			}
			if got := read(t, target); got != originalTarget {
				t.Fatal("adapter error changed destination session")
			}
			if obstruction == "symlink-session" {
				if got, err := os.Readlink(target); err != nil || got != outside {
					t.Fatalf("session symlink changed: %q %v", got, err)
				}
				if got := read(t, outside); got != originalTarget {
					t.Fatal("session import wrote through symlink")
				}
			}
			recovery := sessionRecoveryDirectory(t, p.b)
			recovered := filepath.Join(recovery, "codex", "sessions", "2026", "10", "16", filepath.Base(source))
			if got := read(t, recovered); got != read(t, source) {
				t.Fatal("original authenticated incoming transcript was not recovered")
			}
			for path, permissions := range map[string]os.FileMode{recovery: 0700, recovered: 0600} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != permissions {
					t.Fatalf("fallback recovery is not private: %s %v %v", path, info, err)
				}
			}
			if obstruction == "symlink-session-directory" {
				saved, err := readBaseline(p.bRoot, manifest.ProjectID)
				if err != nil || saved.SnapshotID != manifest.SnapshotID || saved.Fingerprint == "" {
					t.Fatalf("adapter discovery failure prevented workspace receipt: %+v %v", saved, err)
				}
				if got, err := os.Readlink(filepath.Join(p.b.codex, "sessions")); err != nil || got != outside {
					t.Fatalf("session directory symlink changed: %q %v", got, err)
				}
			} else {
				status, err := Status(context.Background(), p.bRoot)
				if err != nil || status.SnapshotID != manifest.SnapshotID || status.Changed {
					t.Fatalf("workspace receipt missing after adapter rejection: %+v %v", status, err)
				}
			}
			assertNoPlaintextStaging(t, p.b)
		})
	}
}

func TestIncomingBranchCannotDiscardIndependentReceiverCommitsAndUndoRestoresRef(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.b.use(t)
	originalMain := git(t, p.bRoot, "rev-parse", "main")
	git(t, p.bRoot, "config", "user.name", "Receiver")
	git(t, p.bRoot, "config", "user.email", "receiver@example.test")
	git(t, p.bRoot, "switch", "-c", "feature")
	write(t, filepath.Join(p.bRoot, "receiver-feature.txt"), "independent receiver commit\n")
	git(t, p.bRoot, "add", "receiver-feature.txt")
	git(t, p.bRoot, "commit", "-m", "receiver feature")
	originalFeature := git(t, p.bRoot, "rev-parse", "feature")
	git(t, p.bRoot, "switch", "main")
	p.a.use(t)
	git(t, p.aRoot, "switch", "-c", "feature")
	write(t, filepath.Join(p.aRoot, "sender-feature.txt"), "independent sender commit\n")
	git(t, p.aRoot, "add", "sender-feature.txt")
	git(t, p.aRoot, "commit", "-m", "sender feature")
	incomingFeature := git(t, p.aRoot, "rev-parse", "feature")
	archive := filepath.Join(p.dir, "branch-conflict.otg")
	if _, err := Export(context.Background(), p.aRoot, archive, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false); err == nil {
		t.Fatal("handoff discarded independent receiver branch without --replace")
	}
	if got := git(t, p.bRoot, "rev-parse", "feature"); got != originalFeature {
		t.Fatal("rejected handoff moved receiver branch")
	}
	if got := git(t, p.bRoot, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatal("rejected handoff switched receiver branch")
	}
	result, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, p.bRoot, "rev-parse", "feature"); got != incomingFeature {
		t.Fatal("explicit replacement did not install incoming branch")
	}
	if _, err := Undo(context.Background(), p.bRoot, result.Backup); err != nil {
		t.Fatal(err)
	}
	if got := git(t, p.bRoot, "rev-parse", "feature"); got != originalFeature {
		t.Fatal("undo did not recover overwritten independent branch ref")
	}
	if got := git(t, p.bRoot, "rev-parse", "main"); got != originalMain {
		t.Fatal("undo changed original current branch ref")
	}
	if got := git(t, p.bRoot, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatal("undo did not restore original current branch")
	}
	if got := git(t, p.bRoot, "show", "feature:receiver-feature.txt"); got != "independent receiver commit" {
		t.Fatal("receiver branch commit content was lost")
	}
	if _, err := os.Stat(filepath.Join(p.bRoot, "sender-feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("undo retained incoming branch workspace file: %v", err)
	}
}

func TestExportDoesNotExceedPendingReceiptLimit(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	var pendingDir string
	for i := 0; i < 64; i++ {
		pending := baseline{SchemaVersion: 2, Root: p.aRoot, ProjectID: cfg.ProjectID, SnapshotID: fmt.Sprintf("%064x", i+1), Fingerprint: strings.Repeat("a", 64), UpdatedAt: time.Now().Add(-time.Duration(i+1) * time.Minute)}
		path, err := pendingPath(p.aRoot, cfg.ProjectID, pending.SnapshotID)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(path, pending); err != nil {
			t.Fatal(err)
		}
		pendingDir = filepath.Dir(path)
	}
	manifest, err := Export(context.Background(), p.aRoot, filepath.Join(p.dir, "pending-limit.otg"), p.b.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 64 {
		t.Fatalf("successful export leaves %d pending receipts; maximum is 64", len(entries))
	}
	latest, err := pendingPath(p.aRoot, cfg.ProjectID, manifest.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(latest); err != nil {
		t.Fatalf("newly exported receipt was pruned: %v", err)
	}
}
