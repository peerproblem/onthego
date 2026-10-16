package capture_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/identity"
)

func TestManagedPathLookupRejectsSymlinkParents(t *testing.T) {
	for _, targetExists := range []bool{true, false} {
		name := "broken"
		if targetExists {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("ONTHEGO_HOME", filepath.Join(base, "data"))
			root := filepath.Join(base, "source")
			mustMkdir(t, root)
			git(t, root, "init")
			git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
			outside := filepath.Join(base, "outside")
			if targetExists {
				mustWrite(t, filepath.Join(outside, "draft.txt"), []byte("outside workspace\n"), 0o600)
			}
			if err := os.Symlink(outside, filepath.Join(root, "notes")); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Exclude = append(cfg.Exclude, "notes")
			options := capture.Options{Root: root, Config: cfg, FingerprintManaged: []string{"notes/draft.txt"}}
			if _, err := capture.Fingerprint(context.Background(), options); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("managed fingerprint accepted %s symlink parent: %v", name, err)
			}
			keys, err := identity.LoadOrCreate()
			if err != nil {
				t.Fatal(err)
			}
			options.FingerprintManaged = nil
			options.PreviousManaged = []string{"notes/draft.txt"}
			options.Keys = keys
			options.Output = filepath.Join(base, "snapshot.otg")
			if _, err := capture.Create(context.Background(), options); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("retention accepted %s symlink parent: %v", name, err)
			}
		})
	}
}

func TestTrackedFingerprintRejectsBrokenSymlinkParent(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "notes", "draft.txt"), []byte("tracked\n"), 0o600)
	git(t, root, "init")
	git(t, root, "add", "notes/draft.txt")
	git(t, root, "-c", "user.name=ONTHEGO Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture")
	if err := os.RemoveAll(filepath.Join(root, "notes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(root, "notes")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Exclude = append(cfg.Exclude, "notes")
	if _, err := capture.Fingerprint(context.Background(), capture.Options{Root: root, Config: cfg}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("tracked fingerprint accepted broken symlink parent: %v", err)
	}
}
