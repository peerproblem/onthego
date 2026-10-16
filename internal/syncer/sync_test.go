package syncer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/restore"
)

type machine struct {
	state, codex, claude string
	keys                 identity.Keys
}

func (m machine) use(t *testing.T) {
	t.Helper()
	t.Setenv("ONTHEGO_HOME", m.state)
	t.Setenv("CODEX_HOME", m.codex)
	t.Setenv("CLAUDE_CONFIG_DIR", m.claude)
}

func newMachine(t *testing.T, dir string) machine {
	t.Helper()
	m := machine{state: filepath.Join(dir, "state"), codex: filepath.Join(dir, "codex"), claude: filepath.Join(dir, "claude")}
	m.use(t)
	var err error
	m.keys, err = identity.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	result, err := gitx.Run(context.Background(), root, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(result.Stdout))
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

func newProject(t *testing.T, root string) config.Config {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-b", "main")
	git(t, root, "config", "user.name", "Sync Test")
	git(t, root, "config", "user.email", "sync@example.test")
	write(t, filepath.Join(root, "file.txt"), "committed\n")
	write(t, filepath.Join(root, ".gitignore"), "ignored.txt\n.env\n.onthego.local/\n*.otg\n")
	cfg := config.Default()
	if err := config.Save(root, cfg); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "initial")
	return cfg
}

func codexSession(t *testing.T, m machine, root, id, text string) string {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": root}})
	message, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"text": text, "model": "gpt-test", "reasoning_effort": "high", "fast": true}})
	path := filepath.Join(m.codex, "sessions", "2026", "10", "16", "rollout-"+id+".jsonl")
	write(t, path, string(meta)+"\n"+string(message)+"\n")
	return path
}

type pair struct {
	dir, aRoot, bRoot string
	a, b              machine
}

func paired(t *testing.T) pair {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := pair{dir: dir, aRoot: filepath.Join(dir, "a", "project"), bRoot: filepath.Join(dir, "b", "project")}
	p.a, p.b = newMachine(t, filepath.Join(dir, "a")), newMachine(t, filepath.Join(dir, "b"))
	p.a.use(t)
	newProject(t, p.aRoot)
	return p
}

func (p pair) firstHandoff(t *testing.T) string {
	t.Helper()
	p.a.use(t)
	path := filepath.Join(p.dir, "first.otg")
	manifest, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	p.a.use(t)
	if err := MarkSynced(context.Background(), p.aRoot, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	return manifest.SnapshotID
}

func TestRoundTripPreservesDirtyGitIgnoredOptInAndNativeSessions(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "file.txt"), "staged\n")
	git(t, p.aRoot, "add", "file.txt")
	write(t, filepath.Join(p.aRoot, "file.txt"), "unstaged\n")
	write(t, filepath.Join(p.aRoot, "untracked.txt"), "untracked\n")
	write(t, filepath.Join(p.aRoot, "ignored.txt"), "opted in\n")
	write(t, filepath.Join(p.aRoot, ".env"), "LOCAL_ONLY=test\n")
	codexSession(t, p.a, p.aRoot, "roundtrip", "keep all metadata")
	path := filepath.Join(p.dir, "initial.otg")
	manifest, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", []string{"ignored.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, err := Status(context.Background(), p.aRoot)
	if err != nil || status.HasBaseline {
		t.Fatalf("unacknowledged export marked clean: %+v %v", status, err)
	}
	p.b.use(t)
	result, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions.Imported != 1 || result.Backup != "" {
		t.Fatalf("unexpected import: %+v", result)
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "unstaged\n" {
		t.Fatal(got)
	}
	if got := git(t, p.bRoot, "show", ":file.txt"); got != "staged" {
		t.Fatal(got)
	}
	if got := read(t, filepath.Join(p.bRoot, "ignored.txt")); got != "opted in\n" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(p.bRoot, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env unexpectedly copied: %v", err)
	}
	status, err = Status(context.Background(), p.bRoot)
	if err != nil || status.Changed || !status.HasBaseline {
		t.Fatalf("restored baseline: %+v %v", status, err)
	}
	importedSession := filepath.Join(p.b.codex, "sessions", "2026", "10", "16", "rollout-roundtrip.jsonl")
	if contents := read(t, importedSession); !strings.Contains(contents, p.bRoot) || !strings.Contains(contents, `"fast":true`) {
		t.Fatal(contents)
	}
	p.a.use(t)
	if err := MarkSynced(context.Background(), p.aRoot, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	status, err = Status(context.Background(), p.aRoot)
	if err != nil || status.Changed {
		t.Fatalf("sender baseline: %+v %v", status, err)
	}
	p.b.use(t)
	write(t, filepath.Join(p.bRoot, "file.txt"), "continued on laptop\n")
	write(t, importedSession, read(t, importedSession)+`{"type":"response_item","payload":{"text":"continued"}}`+"\n")
	back := filepath.Join(p.dir, "back.otg")
	returned, err := Export(context.Background(), p.bRoot, back, p.a.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.a.use(t)
	result, err = Apply(context.Background(), back, p.aRoot, p.b.keys.SigningPublic, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backup == "" || result.Sessions.Updated != 1 {
		t.Fatalf("roundtrip result: %+v", result)
	}
	if got := read(t, filepath.Join(p.aRoot, "file.txt")); got != "continued on laptop\n" {
		t.Fatal(got)
	}
	if !strings.HasPrefix(result.Backup, "refs/onthego/backup/") {
		t.Fatalf("expected Git backup ref, got %q", result.Backup)
	}
	if got := read(t, filepath.Join(p.aRoot, ".env")); got != "LOCAL_ONLY=test\n" {
		t.Fatal("receiver-local environment was removed:", got)
	}
	if got := read(t, filepath.Join(p.aRoot, "ignored.txt")); got != "opted in\n" {
		t.Fatal(got)
	}
	p.b.use(t)
	if err := MarkSynced(context.Background(), p.bRoot, returned.SnapshotID); err != nil {
		t.Fatal(err)
	}
}

func TestLocalChangesConflictAndExplicitReplacementRetainsBackup(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "file.txt"), "incoming\n")
	path := filepath.Join(p.dir, "next.otg")
	if _, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	write(t, filepath.Join(p.bRoot, "file.txt"), "local conflict\n")
	if _, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false); err == nil {
		t.Fatal("local modifications overwritten")
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "local conflict\n" {
		t.Fatal(got)
	}
	result, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "incoming\n" {
		t.Fatal(got)
	}
	if _, err := Undo(context.Background(), p.bRoot, result.Backup); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "local conflict\n" {
		t.Fatal("backup did not restore conflict:", got)
	}
	status, err := Status(context.Background(), p.bRoot)
	if err != nil || status.HasBaseline || !status.Changed {
		t.Fatalf("undo must invalidate delivery receipt: %+v %v", status, err)
	}
}

func TestNewDestinationSessionDoesNotBlockWorkspace(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.a.use(t)
	path := filepath.Join(p.dir, "new-session.otg")
	if _, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	localSession := codexSession(t, p.b, p.bRoot, "destination-only", "local work")
	original := read(t, localSession)
	status, err := Status(context.Background(), p.bRoot)
	if err != nil || !status.Changed {
		t.Fatalf("new native session not marked changed: %+v %v", status, err)
	}
	if _, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatalf("native session blocked independent workspace handoff: %v", err)
	}
	if got := read(t, localSession); got != original {
		t.Fatal("receiver-only session changed")
	}
}

func TestAcknowledgmentPreservesEditsMadeDuringTransfer(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	manifest, err := Export(context.Background(), p.aRoot, filepath.Join(p.dir, "pending.otg"), p.b.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.aRoot, "file.txt"), "edited during transport\n")
	if err := MarkSynced(context.Background(), p.aRoot, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	status, err := Status(context.Background(), p.aRoot)
	if err != nil || !status.Changed || !status.HasBaseline {
		t.Fatalf("in-flight edit marked clean: %+v %v", status, err)
	}
}

func TestStaleSenderCannotOverwriteNewerHandoff(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.a.use(t)
	second := filepath.Join(p.dir, "second.otg")
	if _, err := Export(context.Background(), p.aRoot, second, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), second, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	// Deliberately lose the acknowledgment; the next sender still parents S1.
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "file.txt"), "stale parent\n")
	third := filepath.Join(p.dir, "third.otg")
	if _, err := Export(context.Background(), p.aRoot, third, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), third, p.bRoot, p.a.keys.SigningPublic, false); err == nil {
		t.Fatal("stale ancestor accepted")
	}
}

func TestReplacementCannotCrossProjectIdentity(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	path := filepath.Join(p.dir, "different.otg")
	if _, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	newProject(t, p.bRoot)
	if _, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, true); err == nil || !strings.Contains(err.Error(), "different project") {
		t.Fatalf("project mismatch: %v", err)
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "committed\n" {
		t.Fatal(got)
	}
}

func TestManifestConfigurationIdentityMismatchRejected(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProjectID = "different-project"
	recipient, err := p.b.keys.Recipient()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.dir, "mismatch.otg")
	if _, err := capture.Create(context.Background(), capture.Options{Root: p.aRoot, Config: cfg, Output: path, Recipient: recipient, Keys: p.a.keys}); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false); err == nil || !strings.Contains(err.Error(), "IDs differ") {
		t.Fatalf("manifest mismatch: %v", err)
	}
	if _, err := os.Lstat(p.bRoot); !os.IsNotExist(err) {
		t.Fatalf("failed restore created target: %v", err)
	}
}

func TestSymlinkAncestorsNormalizedAndSensitiveRootsUntrustedSignersRejected(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	link := filepath.Join(p.dir, "link")
	if err := os.Symlink(filepath.Dir(p.aRoot), link); err != nil {
		t.Fatal(err)
	}
	if got, err := safeTarget(filepath.Join(link, "project")); err != nil || got != p.aRoot {
		t.Fatalf("symlink ancestor was not normalized: %q %v", got, err)
	}
	if got, err := safeTarget(filepath.Join(link, "new-project")); err != nil || got != filepath.Join(filepath.Dir(p.aRoot), "new-project") {
		t.Fatalf("missing destination under symlink ancestor: %q %v", got, err)
	}
	home, _ := os.UserHomeDir()
	for _, root := range []string{"/", home, p.a.state, filepath.Dir(p.a.state)} {
		if _, err := safeTarget(root); err == nil {
			t.Fatalf("unsafe root accepted: %s", root)
		}
	}
	if _, err := Apply(context.Background(), "missing.otg", p.bRoot, "", false); err == nil {
		t.Fatal("untrusted signer accepted")
	}
}

func TestLinkedWorktreeInPlaceHandoffPreservesOtherWorktree(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.b.use(t)
	linked := filepath.Join(p.dir, "linked")
	git(t, p.bRoot, "worktree", "add", "-b", "other", linked)
	primaryInfo, err := os.Stat(p.bRoot)
	if err != nil {
		t.Fatal(err)
	}
	linkedInfo, err := os.Stat(linked)
	if err != nil {
		t.Fatal(err)
	}
	gitMarker := read(t, filepath.Join(linked, ".git"))
	p.a.use(t)
	git(t, p.aRoot, "switch", "-c", "other")
	write(t, filepath.Join(p.aRoot, "file.txt"), "incoming to linked worktree\n")
	archive := filepath.Join(p.dir, "linked.otg")
	if _, err := Export(context.Background(), p.aRoot, archive, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), archive, linked, p.a.keys.SigningPublic, true); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(linked, "file.txt")); got != "incoming to linked worktree\n" {
		t.Fatal(got)
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "committed\n" {
		t.Fatal("other worktree changed:", got)
	}
	if got := read(t, filepath.Join(linked, ".git")); got != gitMarker {
		t.Fatal("linked worktree Git marker replaced")
	}
	afterPrimary, err := os.Stat(p.bRoot)
	if err != nil {
		t.Fatal(err)
	}
	afterLinked, err := os.Stat(linked)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(primaryInfo, afterPrimary) || !os.SameFile(linkedInfo, afterLinked) {
		t.Fatal("worktree directory was replaced")
	}
}

func TestTargetLockPreventsConcurrentMutation(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	unlock, err := lock(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Export(context.Background(), p.aRoot, filepath.Join(p.dir, "locked.otg"), p.b.keys.AgeRecipient, "test", nil, nil); err == nil {
		t.Fatal("concurrent export accepted")
	}
	unlock()
	// The inode remains deliberately; only the kernel lock owns exclusivity.
	if _, err := Export(context.Background(), p.aRoot, filepath.Join(p.dir, "after-unlock.otg"), p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatalf("retained lock file blocked retry: %v", err)
	}
}

func TestManagedIgnoredDeletionDoesNotResurrectReceiverCopy(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "ignored.txt"), "synchronized ignored file\n")
	write(t, filepath.Join(p.aRoot, ".env"), "SYNCED_SECRET=test\n")
	first := filepath.Join(p.dir, "ignored-first.otg")
	manifest, err := Export(context.Background(), p.aRoot, first, p.b.keys.AgeRecipient, "test", []string{"ignored.txt"}, []string{".env"})
	if err != nil {
		t.Fatal(err)
	}
	assertSecretNotStoredInGit(t, p.aRoot, "SYNCED_SECRET=test\n")
	p.b.use(t)
	if _, err := Apply(context.Background(), first, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	assertSecretNotStoredInGit(t, p.bRoot, "SYNCED_SECRET=test\n")
	p.a.use(t)
	if err := MarkSynced(context.Background(), p.aRoot, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	for _, name := range []string{"ignored.txt", ".env"} {
		if err := os.Remove(filepath.Join(p.bRoot, name)); err != nil {
			t.Fatal(err)
		}
	}
	second := filepath.Join(p.dir, "ignored-deleted.otg")
	if _, err := Export(context.Background(), p.bRoot, second, p.a.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.a.use(t)
	result, err := Apply(context.Background(), second, p.aRoot, p.b.keys.SigningPublic, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ignored.txt", ".env"} {
		if _, err := os.Lstat(filepath.Join(p.aRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("managed deletion resurrected %s: %v", name, err)
		}
	}
	assertSecretNotStoredInGit(t, p.aRoot, "SYNCED_SECRET=test\n")
	encrypted, err := filepath.Glob(filepath.Join(p.a.state, "backups", "*.age"))
	if err != nil || len(encrypted) == 0 {
		t.Fatalf("secret backup has no encrypted recovery artifact: %v %v", encrypted, err)
	}
	if _, err := restore.Undo(context.Background(), p.aRoot, result.Backup); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"ignored.txt": "synchronized ignored file\n", ".env": "SYNCED_SECRET=test\n"} {
		if got := read(t, filepath.Join(p.aRoot, name)); got != want {
			t.Fatalf("backup did not recover %s: %q", name, got)
		}
	}
	assertSecretNotStoredInGit(t, p.aRoot, "SYNCED_SECRET=test\n")
	if err := restore.PruneBackups(context.Background(), p.aRoot, 0); err != nil {
		t.Fatal(err)
	}
	encrypted, err = filepath.Glob(filepath.Join(p.a.state, "backups", "*.age"))
	if err != nil || len(encrypted) != 0 {
		t.Fatalf("encrypted artifacts remain after undo and backup pruning: %v %v", encrypted, err)
	}

}

func transfer(t *testing.T, dir, name string, from machine, fromRoot string, to machine, toRoot string) {
	t.Helper()
	from.use(t)
	path := filepath.Join(dir, name+".otg")
	manifest, err := Export(context.Background(), fromRoot, path, to.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	to.use(t)
	if _, err := Apply(context.Background(), path, toRoot, from.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	from.use(t)
	if err := MarkSynced(context.Background(), fromRoot, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
}

func TestThreeMachineRotationUsesTransitiveAncestry(t *testing.T) {
	p := paired(t)
	c := newMachine(t, filepath.Join(p.dir, "c"))
	cRoot := filepath.Join(p.dir, "c", "project")
	transfer(t, p.dir, "a-b", p.a, p.aRoot, p.b, p.bRoot)
	write(t, filepath.Join(p.bRoot, "file.txt"), "continued on B\n")
	transfer(t, p.dir, "b-c", p.b, p.bRoot, c, cRoot)
	write(t, filepath.Join(cRoot, "file.txt"), "continued on C\n")
	transfer(t, p.dir, "c-a", c, cRoot, p.a, p.aRoot)
	if got := read(t, filepath.Join(p.aRoot, "file.txt")); got != "continued on C\n" {
		t.Fatal(got)
	}
	transfer(t, p.dir, "a-b-again", p.a, p.aRoot, p.b, p.bRoot)
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "continued on C\n" {
		t.Fatal(got)
	}
}

func TestHubCanSendToDifferentPeersBeforeReturningToFirst(t *testing.T) {
	p := paired(t)
	c := newMachine(t, filepath.Join(p.dir, "c"))
	cRoot := filepath.Join(p.dir, "c", "project")
	transfer(t, p.dir, "hub-a-b", p.a, p.aRoot, p.b, p.bRoot)
	transfer(t, p.dir, "hub-a-c", p.a, p.aRoot, c, cRoot)
	write(t, filepath.Join(p.aRoot, "file.txt"), "hub continued\n")
	transfer(t, p.dir, "hub-a-b-again", p.a, p.aRoot, p.b, p.bRoot)
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "hub continued\n" {
		t.Fatal(got)
	}
}

func TestImportFailurePreservesRecoveryAndDoesNotBlockWorkspace(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "file.txt"), "incoming\n")
	session := codexSession(t, p.a, p.aRoot, "recovery", "must remain recoverable")
	path := filepath.Join(p.dir, "recoverable.otg")
	manifest, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if err := os.MkdirAll(p.b.codex, 0700); err != nil {
		t.Fatal(err)
	}
	unlock, err := fsx.Lock(filepath.Join(p.b.codex, ".onthego-import.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	result, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil {
		t.Fatalf("session import failure blocked workspace: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("session import failure was silent")
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "incoming\n" {
		t.Fatal(got)
	}
	status, err := Status(context.Background(), p.bRoot)
	if err != nil || status.SnapshotID != manifest.SnapshotID || status.Changed {
		t.Fatalf("workspace baseline: %+v %v", status, err)
	}
	recovery := sessionRecoveryDirectory(t, p.b)
	recovered := filepath.Join(recovery, "codex", "sessions", "2026", "10", "16", filepath.Base(session))
	if got := read(t, recovered); got != read(t, session) {
		t.Fatal("recovery changed original authenticated session")
	}
	for path, want := range map[string]os.FileMode{recovery: 0700, recovered: 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("private recovery permissions %s: %v %v", path, info, err)
		}
	}
	assertNoPlaintextStaging(t, p.b)
}

func TestPreviewShowsRememberedSecretSelection(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, ".env"), "SELECTED_SECRET=test\n")
	manifest, err := Export(context.Background(), p.aRoot, filepath.Join(p.dir, "preview.otg"), p.b.keys.AgeRecipient, "test", nil, []string{".env"})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkSynced(context.Background(), p.aRoot, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	files, err := Preview(context.Background(), p.aRoot, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range files {
		if file.RelativePath == ".env" && file.Secret {
			found = true
		}
	}
	if !found {
		t.Fatalf("preview omitted remembered secret: %+v", files)
	}
}

func TestUntrackedExcludedFilesRemainLocalWhileSelectedDeletionPropagates(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "untracked.txt"), "selected source file\n")
	write(t, filepath.Join(p.aRoot, "dist", "local-build.js"), "device-local build\n")
	write(t, filepath.Join(p.aRoot, "auth.json"), `{"device":"local-auth"}`)
	write(t, filepath.Join(p.aRoot, "private.env"), "DEVICE_LOCAL=source\n")
	p.firstHandoff(t)
	p.b.use(t)
	for _, name := range []string{"dist/local-build.js", "auth.json", "private.env"} {
		if _, err := os.Lstat(filepath.Join(p.bRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("unmanaged file transferred: %s %v", name, err)
		}
	}
	if err := os.Remove(filepath.Join(p.bRoot, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	transfer(t, p.dir, "untracked-return", p.b, p.bRoot, p.a, p.aRoot)
	for _, name := range []string{"dist/local-build.js", "auth.json", "private.env"} {
		if _, err := os.Lstat(filepath.Join(p.aRoot, name)); err != nil {
			t.Fatalf("device-local file removed: %s %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(p.aRoot, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("selected deletion resurrected: %v", err)
	}
}
