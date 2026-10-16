package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/config"
)

func TestNPMAutomaticExclusionAndExplicitEncryptedHandoff(t *testing.T) {
	p := paired(t)
	p.a.use(t)
	const token = "//registry.example.invalid/:_authToken=synthetic-r02-regression\n"
	write(t, filepath.Join(p.aRoot, ".npmrc"), token)
	write(t, filepath.Join(p.aRoot, "src", "tokenizer.ts"), "export const tokenizer = true;\n")
	write(t, filepath.Join(p.aRoot, "docs", "secret-handling.md"), "Ordinary source documentation.\n")
	first := filepath.Join(p.dir, "automatic.otg")
	exported, err := ExportWithOptions(context.Background(), p.aRoot, first, p.b.keys.AgeRecipient, "test", ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(exported.Warnings, "\n"), "Secret candidate not transferred: .npmrc") {
		t.Errorf("missing automatic exclusion warning: %v", exported.Warnings)
	}
	assertSecretNotStoredInGit(t, p.aRoot, token)
	p.b.use(t)
	if _, err := Apply(context.Background(), first, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(p.bRoot, ".npmrc")); !os.IsNotExist(err) {
		t.Fatalf("automatically excluded credential was transferred: %v", err)
	}
	for _, path := range []string{"src/tokenizer.ts", "docs/secret-handling.md"} {
		if _, err := os.Stat(filepath.Join(p.bRoot, path)); err != nil {
			t.Fatalf("ordinary file %s did not transfer: %v", path, err)
		}
	}
	assertSecretNotStoredInGit(t, p.bRoot, token)
	p.a.use(t)
	if err := MarkSynced(context.Background(), p.aRoot, exported.SnapshotID); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(p.dir, "explicit.otg")
	if _, err := Export(context.Background(), p.aRoot, second, p.b.keys.AgeRecipient, "test", nil, []string{".npmrc"}); err != nil {
		t.Fatal(err)
	}
	assertSecretNotStoredInGit(t, p.aRoot, token)
	assertNoPlaintextStaging(t, p.a)
	p.b.use(t)
	if _, err := Apply(context.Background(), second, p.bRoot, p.a.keys.SigningPublic, false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(p.bRoot, ".npmrc")); got != token {
		t.Fatalf("encrypted credential changed: %q", got)
	}
	assertSecretNotStoredInGit(t, p.bRoot, token)
	assertNoPlaintextStaging(t, p.b)
}

func TestLegacyAutomaticNPMRCBaselineRemainsCleanAndRetained(t *testing.T) {
	p := paired(t)
	p.firstHandoff(t)
	const token = "legacy synthetic npm credential\n"
	// Synthesize a pre-upgrade receipt: .npmrc was automatically managed and
	// contributed to its fingerprint before the credential policy changed.
	for _, side := range []struct {
		machine machine
		root    string
	}{{p.a, p.aRoot}, {p.b, p.bRoot}} {
		side.machine.use(t)
		write(t, filepath.Join(side.root, ".npmrc"), token)
		cfg, err := config.Load(side.root)
		if err != nil {
			t.Fatal(err)
		}
		state, err := readBaseline(side.root, cfg.ProjectID)
		if err != nil {
			t.Fatal(err)
		}
		state.Managed = unique(append(state.Managed, ".npmrc"))
		state.Fingerprint, err = capture.Fingerprint(context.Background(), capture.Options{Root: side.root, Config: cfg, Include: state.Include, IncludeSecret: state.IncludeSecret, FingerprintManaged: state.Managed, FingerprintSecret: state.IncludeSecret})
		if err != nil {
			t.Fatal(err)
		}
		newPolicyFingerprint, err := capture.Fingerprint(context.Background(), capture.Options{Root: side.root, Config: cfg, Include: state.Include, IncludeSecret: state.IncludeSecret})
		if err != nil || newPolicyFingerprint == state.Fingerprint {
			t.Fatalf("fixture does not distinguish old selection policy: %v", err)
		}
		if err := saveBaseline(state); err != nil {
			t.Fatal(err)
		}
		status, err := Status(context.Background(), side.root)
		if err != nil || status.Changed {
			t.Fatalf("unchanged legacy receipt became dirty after upgrade: %+v %v", status, err)
		}
	}
	p.a.use(t)
	path := filepath.Join(p.dir, "upgraded-policy.otg")
	exported, err := Export(context.Background(), p.aRoot, path, p.b.keys.AgeRecipient, "test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(exported.RetainedPaths, ".npmrc") {
		t.Fatalf("legacy credential was not retained: %v", exported.RetainedPaths)
	}
	for _, file := range exported.Files {
		if file.RelativePath == ".npmrc" {
			t.Fatal("legacy fingerprint opt-in selected credential for export")
		}
	}
	p.b.use(t)
	result, err := Apply(context.Background(), path, p.bRoot, p.a.keys.SigningPublic, false)
	if err != nil {
		t.Fatalf("clean legacy destination required replacement: %v", err)
	}
	if got := read(t, filepath.Join(p.bRoot, ".npmrc")); got != token {
		t.Fatalf("legacy credential bytes changed: %q", got)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "No longer managed; left in place: .npmrc") {
		t.Fatalf("retention is not explained: %v", result.Warnings)
	}
	status, err := Status(context.Background(), p.bRoot)
	if err != nil || status.Changed {
		t.Fatalf("new receiver receipt is not clean: %+v %v", status, err)
	}
	p.a.use(t)
	if err := MarkSynced(context.Background(), p.aRoot, exported.SnapshotID); err != nil {
		t.Fatal(err)
	}
	status, err = Status(context.Background(), p.aRoot)
	if err != nil || status.Changed {
		t.Fatalf("new sender receipt kept legacy fingerprint selection: %+v %v", status, err)
	}
}
