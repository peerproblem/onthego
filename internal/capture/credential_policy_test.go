package capture_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/config"
)

func TestCredentialCandidatesRequireSecretOptIn(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	candidates := []string{".npmrc", ".yarnrc", ".yarnrc.yml", ".pypirc", ".netrc", "_netrc", ".git-credentials", ".pgpass", ".envrc", ".dockercfg", "kubeconfig", "client.pfx", "signing.jks", "app.keystore", "infra.tfvars", "infra.tfstate", "infra.tfstate.backup"}
	ordinary := []string{"src/tokenizer.ts", "docs/secret-handling.md", "slides.key", "npmrc.ts", "app.tfstate.json"}
	for _, path := range append(append([]string{}, candidates...), ordinary...) {
		mustWrite(t, filepath.Join(root, path), []byte("synthetic _authToken=regression-only\n"), 0o600)
	}
	files, err := capture.Preview(context.Background(), capture.Options{Root: root, Config: config.Default()})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]capture.PreviewFile{}
	for _, file := range files {
		byPath[file.RelativePath] = file
	}
	for _, path := range candidates {
		file, ok := byPath[path]
		if !ok || !file.Excluded || !file.Secret || !strings.Contains(file.Policy, "secret candidate") {
			t.Errorf("credential candidate %s was selected or not explained: %+v", path, file)
		}
	}
	for _, path := range ordinary {
		file, ok := byPath[path]
		if !ok || file.Excluded || file.Secret || file.Policy != "untracked" {
			t.Errorf("ordinary file %s was blocked: %+v", path, file)
		}
	}
}

func TestLegacyFingerprintPreservesSecretModeAndOmitsRemovedFiles(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	path := filepath.Join(root, ".env")
	mustWrite(t, path, []byte("SYNTHETIC=private\n"), 0o700)
	cfg := config.Default()
	options := capture.Options{Root: root, Config: cfg, IncludeSecret: []string{".env"}}
	initial, err := capture.Fingerprint(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	options.Config.Exclude = append(options.Config.Exclude, ".env")
	options.IncludeSecret = nil
	options.FingerprintManaged = []string{".env"}
	options.FingerprintSecret = []string{".env"}
	legacy, err := capture.Fingerprint(context.Background(), options)
	if err != nil || legacy != initial {
		t.Fatalf("legacy secret mode changed: %s %s %v", legacy, initial, err)
	}
	files, err := capture.Preview(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.RelativePath == ".env" && !file.Excluded {
			t.Fatal("fingerprint compatibility selected excluded secret")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	missing, err := capture.Fingerprint(context.Background(), options)
	if err != nil || missing == legacy {
		t.Fatalf("deleted legacy path is not omitted: %s %s %v", missing, legacy, err)
	}
}

func TestIgnoredCredentialCandidatesAreExplainedWithoutWalkingIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	mustWrite(t, filepath.Join(root, ".gitignore"), []byte(".env\n.npmrc\nnode_modules/\n"), 0o600)
	mustWrite(t, filepath.Join(root, ".env"), []byte("SYNTHETIC=value\n"), 0o600)
	mustWrite(t, filepath.Join(root, ".npmrc"), []byte("synthetic-auth-token\n"), 0o600)
	mustWrite(t, filepath.Join(root, "node_modules", ".env"), []byte("not traversed\n"), 0o600)
	files, err := capture.Preview(context.Background(), capture.Options{Root: root, Config: config.Default()})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]capture.PreviewFile{}
	for _, file := range files {
		byPath[file.RelativePath] = file
	}
	for _, path := range []string{".env", ".npmrc"} {
		file, ok := byPath[path]
		if !ok || !file.Excluded || file.Policy != "ignored secret candidate; not transferred" {
			t.Errorf("ignored candidate %s is not explained: %+v", path, file)
		}
	}
	if _, ok := byPath["node_modules/.env"]; ok {
		t.Fatal("ignored directory contents were enumerated")
	}
}
