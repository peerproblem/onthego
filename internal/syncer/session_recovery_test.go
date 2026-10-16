package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/restore"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplayRetriesFailedSessionsWithoutChangingWorkspace(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	source := codexSession(t, p.a, p.aRoot, "retry", "recoverable history")
	archive := filepath.Join(p.dir, "retry.otg")
	if _, err := Export(context.Background(), p.aRoot, archive, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	obstruction := filepath.Join(p.b.codex, ".onthego-import.lock")
	if err := os.MkdirAll(obstruction, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.bRoot, "file.txt"), "local work after handoff\n")
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyApplied || result.Sessions.Imported != 1 {
		t.Fatalf("sessions were not retried: %+v", result)
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "local work after handoff\n" {
		t.Fatal("workspace edits overwritten")
	}
	if _, err := os.Stat(filepath.Join(p.b.codex, "sessions", "2026", "10", "16", filepath.Base(source))); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverSessionsWithoutPackagePreservesEdits(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	source := codexSession(t, p.a, p.aRoot, "offline-recovery", "saved incoming history")
	archive := filepath.Join(p.dir, "offline-recovery.otg")
	if _, err := Export(context.Background(), p.aRoot, archive, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	obstruction := filepath.Join(p.b.codex, ".onthego-import.lock")
	if err := os.MkdirAll(obstruction, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil || first.Sessions.Status != "recovery_needed" {
		t.Fatalf("first: %+v %v", first, err)
	}
	recovery := sessionRecoveryDirectory(t, p.b)
	status, err := Status(context.Background(), p.bRoot)
	if err != nil || !status.SessionRecoveryPending {
		t.Fatalf("status: %+v %v", status, err)
	}
	baselineBefore, err := readBaseline(p.bRoot, first.Manifest.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.bRoot, "file.txt"), "work must survive recovery\n")
	result, err := RecoverSessions(context.Background(), p.bRoot, agentsessions.ImportOptions{})
	if err != nil || !result.SessionsRetried || result.Sessions.Imported != 1 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "work must survive recovery\n" {
		t.Fatal(got)
	}
	if _, err := os.Stat(recovery); !os.IsNotExist(err) {
		t.Fatalf("recovery not cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.b.codex, "sessions", "2026", "10", "16", filepath.Base(source))); err != nil {
		t.Fatal(err)
	}
	after, err := readBaseline(p.bRoot, first.Manifest.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Fingerprint != baselineBefore.Fingerprint || after.SnapshotID != baselineBefore.SnapshotID {
		t.Fatal("workspace receipt changed during session-only recovery")
	}
	status, err = Status(context.Background(), p.bRoot)
	if err != nil || status.SessionRecoveryPending || !status.Changed {
		t.Fatalf("recovered status: %+v %v", status, err)
	}
}

func TestPruneBackupsObeysWorkspaceLock(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	p.b.use(t)
	root, err := safeTarget(p.bRoot)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	before := git(t, p.bRoot, "for-each-ref", "refs/onthego/backup")
	err = PruneBackups(context.Background(), p.bRoot, 0)
	var typed *otgerror.Error
	if !errors.As(err, &typed) || typed.Code != otgerror.CodePrecondition {
		t.Fatalf("prune ignored lock: %v", err)
	}
	if after := git(t, p.bRoot, "for-each-ref", "refs/onthego/backup"); after != before {
		t.Fatal("prune modified refs while locked")
	}
}

func TestUndoWorksWithoutValidPolicy(t *testing.T) {
	for _, broken := range []string{"missing", "yaml", "project-id"} {
		t.Run(broken, func(t *testing.T) {
			p := paired(t)
			p.firstHandoff(t)
			p.a.use(t)
			write(t, filepath.Join(p.aRoot, "file.txt"), "incoming second handoff\n")
			transfer(t, p.dir, "second", p.a, p.aRoot, p.b, p.bRoot)
			p.b.use(t)
			backups, err := restore.Backups(context.Background(), p.bRoot)
			if err != nil || len(backups) == 0 {
				t.Fatalf("backups: %v %v", backups, err)
			}
			path := filepath.Join(p.bRoot, config.FileName)
			switch broken {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "yaml":
				write(t, path, "schema_version: [broken\n")
			case "project-id":
				write(t, path, "schema_version: 2\nproject_id: ../bad\nagent: auto\n")
			}
			if _, err := Undo(context.Background(), p.bRoot, backups[0].Ref); err != nil {
				t.Fatal(err)
			}
			if got := read(t, filepath.Join(p.bRoot, "file.txt")); got != "committed\n" {
				t.Fatal(got)
			}
			if _, err := config.Load(p.bRoot); err != nil {
				t.Fatal("policy not restored:", err)
			}
		})
	}
}

func TestSessionRecoveryAfterPartialAdapterSuccess(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	source := codexSession(t, p.a, p.aRoot, "already-imported", "preserve completed adapter")
	claude, err := json.Marshal(map[string]any{"type": "user", "sessionId": "pending-claude", "cwd": p.aRoot, "message": map[string]any{"role": "user", "content": "restore other adapter"}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.a.claude, "projects", agentsessions.ProjectDirectory(p.aRoot), "pending-claude.jsonl"), string(claude)+"\n")
	archive := filepath.Join(p.dir, "partial-adapter.otg")
	if _, err := Export(context.Background(), p.aRoot, archive, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	obstruction := filepath.Join(p.b.claude, ".onthego-import.lock")
	if err := os.MkdirAll(obstruction, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), archive, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	// Model the completed portion of an interrupted multi-adapter import. Replaying
	// its identical bytes must skip it while installing the remaining adapter.
	target := filepath.Join(p.b.codex, "sessions", "2026", "10", "16", filepath.Base(source))
	write(t, target, strings.ReplaceAll(read(t, source), p.aRoot, p.bRoot))
	original := read(t, target)
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	result, err := RecoverSessions(context.Background(), p.bRoot, agentsessions.ImportOptions{})
	if err != nil || result.Sessions.Imported != 1 || result.Sessions.Skipped != 1 {
		t.Fatalf("partial retry: %+v %v", result, err)
	}
	if read(t, target) != original {
		t.Fatal("completed adapter history changed")
	}
	if _, err := os.Stat(filepath.Join(p.b.claude, "projects", agentsessions.ProjectDirectory(p.bRoot), "pending-claude.jsonl")); err != nil {
		t.Fatal(err)
	}
}
