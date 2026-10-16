package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peerproblem/onthego/internal/config"
)

func TestManagedDirectoryReplacedByFileHandoff(t *testing.T) {
	ctx := context.Background()
	p := paired(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "notes", "draft.txt"), "old draft\n")
	p.firstHandoff(t)
	p.a.use(t)
	if err := os.RemoveAll(filepath.Join(p.aRoot, "notes")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.aRoot, "notes"), "replacement file\n")
	status, err := Status(ctx, p.aRoot)
	if err != nil || !status.Changed {
		t.Fatalf("directory replacement is not reported as changed: %+v %v", status, err)
	}
	path := filepath.Join(p.dir, "directory-to-file.otg")
	manifest, err := Export(ctx, p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatalf("export directory replacement: %v", err)
	}
	if contains(manifest.RetainedPaths, "notes/draft.txt") {
		t.Fatal("deleted child was retained")
	}
	p.b.use(t)
	if _, err := Apply(ctx, path, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatalf("apply directory replacement: %v", err)
	}
	if got := read(t, filepath.Join(p.bRoot, "notes")); got != "replacement file\n" {
		t.Fatalf("replacement file differs: %q", got)
	}
	cfg, err := config.Load(p.bRoot)
	if err != nil {
		t.Fatal(err)
	}
	state, err := readBaseline(p.bRoot, cfg.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if contains(state.Managed, "notes/draft.txt") || !contains(state.Managed, "notes") {
		t.Fatalf("receiver managed paths do not match replacement: %v", state.Managed)
	}
	status, err = Status(ctx, p.bRoot)
	if err != nil || status.Changed {
		t.Fatalf("receiver changed after apply: %+v %v", status, err)
	}
	p.a.use(t)
	if err := MarkSynced(ctx, p.aRoot, manifest.SnapshotID); err != nil {
		t.Fatal(err)
	}
	status, err = Status(ctx, p.aRoot)
	if err != nil || status.Changed {
		t.Fatalf("sender changed after acknowledgement: %+v %v", status, err)
	}
}
