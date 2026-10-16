package packageotg

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func packageFixture(t *testing.T, entries map[string][]byte) (string, snapshot.Manifest, *age.X25519Identity, ed25519.PrivateKey) {
	t.Helper()
	root := t.TempDir()
	manifest := snapshot.Manifest{SchemaVersion: snapshot.SchemaVersion, ProjectID: "roundtrip", SourceGitHead: strings.Repeat("a", 40), PayloadHashes: map[string]string{}, Git: snapshot.Git{WIPCommit: strings.Repeat("b", 40), IndexTree: strings.Repeat("c", 40), WorktreeTree: strings.Repeat("d", 40), UntrackedTree: strings.Repeat("e", 40)}}
	for name, data := range entries {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		manifest.PayloadHashes[name] = hex.EncodeToString(hash[:])
	}
	manifest.Git.BundleSHA256 = manifest.PayloadHashes["repo.bundle"]
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return root, manifest, identity, signer
}

func rawPackage(t *testing.T, identity *age.X25519Identity, write func(*tar.Writer)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.otg")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := age.Encrypt(file, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := zstd.NewWriter(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(compressed)
	write(archive)
	for _, closer := range []io.Closer{archive, compressed, encrypted, file} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func signedManifest(t *testing.T, manifest snapshot.Manifest, signer ed25519.PrivateKey) ([]byte, []byte) {
	t.Helper()
	manifest.SignerPublic = base64.StdEncoding.EncodeToString(signer.Public().(ed25519.PublicKey))
	if err := manifest.SetSnapshotID(); err != nil {
		t.Fatal(err)
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(signer, canonical)))
	return data, signature
}

func TestHighlyCompressiblePayloadRoundTrip(t *testing.T) {
	for _, name := range []string{"context/native-sessions/codex/session.jsonl", "secret-files/.npmrc"} {
		t.Run(name, func(t *testing.T) {
			content := []byte(strings.Repeat("synthetic repeated session or secret content\n", 50000))
			payload, manifest, identity, signer := packageFixture(t, map[string][]byte{"repo.bundle": []byte("bundle"), name: content})
			path := filepath.Join(t.TempDir(), "test.otg")
			if err := Create(payload, path, manifest, identity.Recipient(), signer); err != nil {
				t.Fatal(err)
			}
			out := t.TempDir()
			if _, err := Extract(path, out, identity, ""); err != nil {
				t.Fatalf("Create succeeded but Extract failed: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(name)))
			if err != nil || string(data) != string(content) {
				t.Fatalf("roundtrip changed bytes: %v", err)
			}
		})
	}
}

func TestBadSignatureRejectedBeforePayloadWrite(t *testing.T) {
	payload, manifest, identity, signer := packageFixture(t, map[string][]byte{"repo.bundle": []byte("bundle"), "context/data": []byte("context")})
	data, signature := signedManifest(t, manifest, signer)
	signature[0] = 'A'
	if signature[1] == 'A' {
		signature[1] = 'B'
	} else {
		signature[1] = 'A'
	}
	path := rawPackage(t, identity, func(w *tar.Writer) {
		if err := writeBytes(w, "manifest.json", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeBytes(w, "manifest.sig", signature, 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeTree(w, payload); err != nil {
			t.Fatal(err)
		}
	})
	out := t.TempDir()
	if _, err := Extract(path, out, identity, ""); err == nil {
		t.Fatal("accepted bad signature")
	}
	if _, err := os.Lstat(filepath.Join(out, "repo.bundle")); !os.IsNotExist(err) {
		t.Fatalf("unauthenticated payload reached disk: %v", err)
	}
}

func TestCreateAndExtractShareAbsolutePayloadBoundary(t *testing.T) {
	payload, manifest, identity, signer := packageFixture(t, map[string][]byte{"repo.bundle": []byte(strings.Repeat("bundle", 10000)), "context/data": []byte(strings.Repeat("content", 1000))})
	var err error
	manifest.PayloadSizes, err = PayloadSizesContext(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	data, signature := signedManifest(t, manifest, signer)
	exact := int64(len(data)+len(signature)) + manifest.PayloadSizes["context/data"]
	original := MaxPayloadSize
	t.Cleanup(func() { MaxPayloadSize = original })
	for _, delta := range []int64{1, 0, -1} {
		MaxPayloadSize = exact + delta
		path := filepath.Join(t.TempDir(), "test.otg")
		err := Create(payload, path, manifest, identity.Recipient(), signer)
		if delta < 0 {
			if err == nil {
				t.Fatal("pack accepted one byte over shared limit")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("oversized package published: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("pack rejected boundary delta %d: %v", delta, err)
		}
		if _, err := Extract(path, t.TempDir(), identity, ""); err != nil {
			t.Fatalf("extract rejected boundary delta %d: %v", delta, err)
		}
	}
}

func TestExtractAcceptsSchema2WithoutNewFields(t *testing.T) {
	payload, manifest, identity, signer := packageFixture(t, map[string][]byte{"repo.bundle": []byte("bundle"), "context/data": []byte("context")})
	manifest.SchemaVersion = 2
	data, signature := signedManifest(t, manifest, signer)
	if strings.Contains(string(data), "payload_sizes") || strings.Contains(string(data), "retained_paths") {
		t.Fatal("schema 2 canonical representation gained fields")
	}
	path := rawPackage(t, identity, func(w *tar.Writer) {
		if err := writeBytes(w, "manifest.json", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeBytes(w, "manifest.sig", signature, 0600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"context", "empty-legacy-directory"} {
			if err := w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
				t.Fatal(err)
			}
		}
		if err := writeTree(w, payload); err != nil {
			t.Fatal(err)
		}
	})
	out := t.TempDir()
	if _, err := Extract(path, out, identity, ""); err != nil {
		t.Fatalf("schema 2 package rejected: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(out, "empty-legacy-directory")); !os.IsNotExist(err) {
		t.Fatalf("unsigned schema 2 directory was materialized: %v", err)
	}
}

func TestExtractRejectsSignedSizeMismatchBeforeEntryWrite(t *testing.T) {
	for _, name := range []string{"repo.bundle", "context/data"} {
		t.Run(name, func(t *testing.T) {
			payload, manifest, identity, signer := packageFixture(t, map[string][]byte{"repo.bundle": []byte("bundle"), "context/data": []byte("context")})
			var err error
			manifest.PayloadSizes, err = PayloadSizesContext(context.Background(), payload)
			if err != nil {
				t.Fatal(err)
			}
			manifest.PayloadSizes[name]++
			data, signature := signedManifest(t, manifest, signer)
			path := rawPackage(t, identity, func(w *tar.Writer) {
				if err := writeBytes(w, "manifest.json", data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := writeBytes(w, "manifest.sig", signature, 0600); err != nil {
					t.Fatal(err)
				}
				if err := writeTree(w, payload); err != nil {
					t.Fatal(err)
				}
			})
			out := t.TempDir()
			if _, err := Extract(path, out, identity, ""); err == nil || !strings.Contains(err.Error(), "size does not match") {
				t.Fatalf("expected signed size failure: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(out, filepath.FromSlash(name))); !os.IsNotExist(err) {
				t.Fatalf("size-mismatched entry reached disk: %v", err)
			}
		})
	}
}

func TestExtractRequiresBoundedManifestAndSignatureFirst(t *testing.T) {
	_, manifest, identity, signer := packageFixture(t, map[string][]byte{"repo.bundle": []byte("bundle")})
	data, signature := signedManifest(t, manifest, signer)
	for _, test := range []string{"payload first", "signature first", "large manifest", "large signature"} {
		t.Run(test, func(t *testing.T) {
			path := rawPackage(t, identity, func(w *tar.Writer) {
				switch test {
				case "payload first":
					if err := writeBytes(w, "repo.bundle", []byte("bundle"), 0600); err != nil {
						t.Fatal(err)
					}
				case "signature first":
					if err := writeBytes(w, "manifest.sig", signature, 0600); err != nil {
						t.Fatal(err)
					}
				case "large manifest":
					if err := writeBytes(w, "manifest.json", make([]byte, MaxManifestSize+1), 0600); err != nil {
						t.Fatal(err)
					}
				case "large signature":
					if err := writeBytes(w, "manifest.json", data, 0600); err != nil {
						t.Fatal(err)
					}
					if err := writeBytes(w, "manifest.sig", make([]byte, MaxManifestSize+1), 0600); err != nil {
						t.Fatal(err)
					}
				}
			})
			out := t.TempDir()
			if _, err := Extract(path, out, identity, ""); err == nil {
				t.Fatal("accepted invalid initial entries")
			}
			entries, err := os.ReadDir(out)
			if err != nil || len(entries) != 0 {
				t.Fatalf("wrote unauthenticated archive content: %v, %v", entries, err)
			}
		})
	}
}

func TestRetainedPathsValidation(t *testing.T) {
	payload, manifest, _, _ := packageFixture(t, map[string][]byte{"repo.bundle": []byte("bundle")})
	var err error
	manifest.PayloadSizes, err = PayloadSizesContext(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{{"../escape"}, {".git/config"}, {"same", "same"}, make([]string, MaxFiles+1)} {
		manifest.RetainedPaths = paths
		if err := validateManifest(manifest); err == nil {
			t.Fatalf("accepted retained paths: %v", paths)
		}
	}
	manifest.RetainedPaths = []string{"notes.txt"}
	if err := validateManifest(manifest); err != nil {
		t.Fatalf("valid retained path rejected: %v", err)
	}
	manifest.Files = []snapshot.File{{RelativePath: "notes.txt", Kind: "file", SHA256: strings.Repeat("a", 64)}}
	if err := validateManifest(manifest); err == nil {
		t.Fatal("retained path overlaps selected file")
	}
}
