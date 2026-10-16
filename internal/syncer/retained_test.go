package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/config"
)

func TestPolicyRemovalRetainsReceiverFile(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "notes.txt"), "keep these bytes\n")
	p.firstHandoff(t)
	p.a.use(t)
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Exclude = append(cfg.Exclude, "notes.txt")
	if err := config.Save(p.aRoot, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.dir, "excluded.otg")
	if _, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil); err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	result, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(p.bRoot, "notes.txt")); err != nil || string(data) != "keep these bytes\n" {
		t.Fatalf("unmanaged file lost: %q %v", data, err)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "No longer managed; left in place: notes.txt") {
		t.Fatalf("missing retention warning: %v", result.Warnings)
	}
	state, err := readBaseline(p.bRoot, cfg.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if contains(state.Managed, "notes.txt") {
		t.Fatal("retained file remains managed")
	}
}

func TestSourceDeletionStillDeletesReceiverFile(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	write(t, filepath.Join(p.aRoot, "notes.txt"), "delete me\n")
	p.firstHandoff(t)
	p.a.use(t)
	if err := os.Remove(filepath.Join(p.aRoot, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	transfer(t, p.dir, "deleted", p.a, p.aRoot, p.b, p.bRoot)
	if _, err := os.Stat(filepath.Join(p.bRoot, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted source survived: %v", err)
	}
}

func TestExcludedPersistedSecretIsRetained(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	const secret = "synthetic-sensitive-value\n"
	write(t, filepath.Join(p.aRoot, ".env"), secret)
	first := filepath.Join(p.dir, "secret-first.otg")
	initial, err := Export(context.Background(), p.aRoot, first, p.b.keys.AgeRecipient, "test", nil, []string{".env"})
	if err != nil {
		t.Fatal(err)
	}
	p.b.use(t)
	if _, err := Apply(context.Background(), first, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	p.a.use(t)
	if err := MarkSynced(context.Background(), p.aRoot, initial.SnapshotID); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p.aRoot)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Exclude = append(cfg.Exclude, ".env")
	if err := config.Save(p.aRoot, cfg); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(p.dir, "secret-excluded.otg")
	exported, err := ExportWithOptions(context.Background(), p.aRoot, next, p.b.keys.AgeRecipient, "test", ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(exported.RetainedPaths, ".env") {
		t.Fatal("secret missing from retained paths")
	}
	for _, f := range exported.Files {
		if f.RelativePath == ".env" {
			t.Fatal("excluded secret in payload")
		}
	}
	if !strings.Contains(strings.Join(exported.Warnings, "\n"), "Previously included secret excluded") {
		t.Fatal(exported.Warnings)
	}
	if _, err := Export(context.Background(), p.aRoot, filepath.Join(p.dir, "explicit-conflict.otg"), p.b.keys.AgeRecipient, "test", nil, []string{".env"}); err == nil {
		t.Fatal("explicit contradictory selection accepted")
	}
	assertSecretNotStoredInGit(t, p.aRoot, secret)
	p.b.use(t)
	result, err := Apply(context.Background(), next, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(p.bRoot, ".env")); got != secret {
		t.Fatal("secret bytes changed")
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "No longer managed; left in place: .env") {
		t.Fatal(result.Warnings)
	}
	assertSecretNotStoredInGit(t, p.bRoot, secret)
}
