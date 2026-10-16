package syncer

import (
	"context"
	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func TestUnacknowledgedCredentialReturnPreservesRetainedFile(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	ctx := context.Background()
	for _, side := range []struct {
		m    machine
		root string
	}{{p.a, p.aRoot}, {p.b, p.bRoot}} {
		side.m.use(t)
		write(t, filepath.Join(side.root, ".npmrc"), "legacy token\n")
		cfg, e := config.Load(side.root)
		if e != nil {
			t.Fatal(e)
		}
		b, e := readBaseline(side.root, cfg.ProjectID)
		if e != nil {
			t.Fatal(e)
		}
		b.Managed = append(b.Managed, ".npmrc")
		b.Fingerprint, e = capture.Fingerprint(ctx, optionsFor(side.root, cfg, b))
		if e != nil {
			t.Fatal(e)
		}
		if e = saveBaseline(b); e != nil {
			t.Fatal(e)
		}
	}
	p.a.use(t)
	upgrade := filepath.Join(p.dir, "upgrade.otg")
	m, e := Export(ctx, p.aRoot, upgrade, p.b.keys.AgeRecipient, "test", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Log("outbound retained", m.RetainedPaths)
	p.b.use(t)
	if _, e = Apply(ctx, upgrade, p.bRoot, p.a.keys.SigningPublic, false); e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(p.bRoot, "file.txt"), "return work\n")
	returned := filepath.Join(p.dir, "return.otg")
	m, e = Export(ctx, p.bRoot, returned, p.a.keys.AgeRecipient, "test", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Log("return retained", m.RetainedPaths)
	p.a.use(t)
	r, e := Apply(ctx, returned, p.aRoot, p.b.keys.SigningPublic, false)
	if e != nil {
		t.Fatal(e)
	}
	t.Log("return result warnings", r.Warnings)
	if _, e = os.Stat(filepath.Join(p.aRoot, ".npmrc")); e != nil {
		t.Fatalf("local retained credential lost during unacknowledged return: %v", e)
	}
}

func TestRecoverSessionsKeepsConsecutiveFailedDeliveries(t *testing.T) {
	p := paired(t)
	ctx := context.Background()
	p.a.use(t)
	first := codexSession(t, p.a, p.aRoot, "older-history", "older missing history")
	second := codexSession(t, p.a, p.aRoot, "latest-history", "latest missing history")
	p.b.use(t)
	obstruction := filepath.Join(p.b.codex, ".onthego-import.lock")
	if e := os.MkdirAll(obstruction, 0700); e != nil {
		t.Fatal(e)
	}
	for i, id := range []string{"older-history", "latest-history"} {
		p.a.use(t)
		write(t, filepath.Join(p.aRoot, "file.txt"), id+"\n")
		archive := filepath.Join(p.dir, id+".otg")
		m, e := ExportWithOptions(ctx, p.aRoot, archive, p.b.keys.AgeRecipient, "test", ExportOptions{Selection: agentsessions.Selection{IDs: []string{id}}})
		if e != nil {
			t.Fatal(e)
		}
		p.b.use(t)
		r, e := Apply(ctx, archive, p.bRoot, p.a.keys.SigningPublic, false)
		if e != nil || r.Sessions.Status != "recovery_needed" {
			t.Fatalf("delivery %d: %+v %v", i, r, e)
		}
		p.a.use(t)
		if e = MarkSynced(ctx, p.aRoot, m.SnapshotID); e != nil {
			t.Fatal(e)
		}
	}
	p.b.use(t)
	if e := os.Remove(obstruction); e != nil {
		t.Fatal(e)
	}
	result, e := RecoverSessions(ctx, p.bRoot, agentsessions.ImportOptions{})
	if e != nil {
		t.Fatal(e)
	}
	t.Log("recovery", result.Sessions)
	status, e := Status(ctx, p.bRoot)
	if e != nil {
		t.Fatal(e)
	}
	t.Log("pending", status.SessionRecoveryPending)
	if _, e = os.Stat(filepath.Join(p.b.codex, "sessions", "2026", "10", "16", filepath.Base(second))); e != nil {
		t.Fatal("latest not recovered", e)
	}
	if _, e = os.Stat(filepath.Join(p.b.codex, "sessions", "2026", "10", "16", filepath.Base(first))); e != nil {
		t.Fatalf("older failed recovery lost from queue while status pending=%v: %v", status.SessionRecoveryPending, e)
	}
}
