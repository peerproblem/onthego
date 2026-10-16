package packageotg

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func TestCanceledPackageOperationsDoNotStartIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	output := filepath.Join(root, "out.otg")
	if err := CreateContext(ctx, "does-not-exist", output, snapshot.Manifest{}, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("create ignored cancellation: %v", err)
	}
	if _, err := ExtractContext(ctx, "does-not-exist", filepath.Join(root, "out"), nil, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("extract ignored cancellation: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled operations created files: %v, %v", entries, err)
	}
}

func TestCreateCancellationDuringEncryptionDoesNotPublish(t *testing.T) {
	root, payload, manifest, key, signer := cancellationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := cancelAfterProgress(ctx, cancel, filepath.Join(root, ".onthego-package-*.tmp"))
	output := filepath.Join(root, "out.otg")
	err := CreateContext(ctx, payload, output, manifest, key.Recipient(), signer)
	cancel()
	if !<-observed {
		t.Fatal("did not interrupt an active encrypted write")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create did not stop during encryption: %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("canceled package was published")
	}
	remaining, err := filepath.Glob(filepath.Join(root, ".onthego-package-*.tmp"))
	if err != nil || len(remaining) != 0 {
		t.Fatalf("partial package was retained: %v, %v", remaining, err)
	}
}

func TestExtractCancellationDuringPlaintextCopyStopsPromptly(t *testing.T) {
	root, payload, manifest, key, signer := cancellationFixture(t)
	archive := filepath.Join(root, "in.otg")
	if err := Create(payload, archive, manifest, key.Recipient(), signer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := filepath.Join(root, "extracted")
	observed := cancelAfterProgress(ctx, cancel, filepath.Join(output, "repo.bundle"))
	_, err := ExtractContext(ctx, archive, output, key, base64.StdEncoding.EncodeToString(signer.Public().(ed25519.PublicKey)))
	cancel()
	if !<-observed {
		t.Fatal("did not interrupt an active plaintext copy")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("extract did not stop during plaintext copy: %v", err)
	}
	info, err := os.Stat(filepath.Join(output, "repo.bundle"))
	if err != nil || info.Size() >= 16<<20 {
		t.Fatalf("extract completed the payload after cancellation: %v", err)
	}
	// Extraction callers own their private temporary directory and remove it on
	// failure. Main's subprocess test checks this lifecycle in an actual handoff.
	if err := os.RemoveAll(output); err != nil {
		t.Fatal(err)
	}
}

func cancellationFixture(t *testing.T) (string, string, snapshot.Manifest, *age.X25519Identity, ed25519.PrivateKey) {
	t.Helper()
	root := t.TempDir()
	payload := filepath.Join(root, "payload")
	if err := os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 16<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, "repo.bundle"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	manifest := snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion, ProjectID: "context-test", SourceGitHead: strings.Repeat("a", 40),
		PayloadHashes: map[string]string{"repo.bundle": digest},
		Git:           snapshot.Git{BundleSHA256: digest, WIPCommit: strings.Repeat("b", 40), IndexTree: strings.Repeat("c", 40), WorktreeTree: strings.Repeat("d", 40), UntrackedTree: strings.Repeat("e", 40)},
	}
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return root, payload, manifest, key, signer
}

func cancelAfterProgress(ctx context.Context, cancel context.CancelFunc, pattern string) <-chan bool {
	observed := make(chan bool, 1)
	go func() {
		tick := time.NewTicker(100 * time.Microsecond)
		defer tick.Stop()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case <-tick.C:
				paths, _ := filepath.Glob(pattern)
				for _, path := range paths {
					if info, err := os.Stat(path); err == nil && info.Size() > 256<<10 {
						cancel()
						observed <- true
						return
					}
				}
			case <-ctx.Done():
				observed <- false
				return
			case <-deadline.C:
				cancel()
				observed <- false
				return
			}
		}
	}()
	return observed
}
