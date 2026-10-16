package packageotg

import (
	"archive/tar"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/jbaehova/onthego/internal/otgerror"
	"github.com/jbaehova/onthego/internal/snapshot"
	"github.com/klauspost/compress/zstd"
)

func TestExtractRejectsCiphertextTampering(t *testing.T) {
	base := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "broken.otg")
	if err := os.WriteFile(path, []byte("not an age package"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Extract(path, filepath.Join(base, "out"), identity, "")
	var typed *otgerror.Error
	if !errors.As(err, &typed) || typed.Code != otgerror.CodePackageInvalid {
		t.Fatalf("expected E_PACKAGE_INVALID, got %v", err)
	}
}

func TestExtractRejectsSymlinkAncestorChain(t *testing.T) {
	base := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "chain.otg")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ageWriter, err := age.Encrypt(file, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	zstdWriter, err := zstd.NewWriter(ageWriter)
	if err != nil {
		t.Fatal(err)
	}
	tarWriter := tar.NewWriter(zstdWriter)
	for _, header := range []*tar.Header{
		{Name: "files/a", Typeflag: tar.TypeSymlink, Linkname: ".."},
		{Name: "files/b", Typeflag: tar.TypeSymlink, Linkname: "a/.."},
		{Name: "files/b/escaped", Typeflag: tar.TypeReg, Mode: 0o600, Size: 7},
	} {
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tarWriter.Write([]byte("escaped")); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, closer := range []interface{ Close() error }{tarWriter, zstdWriter, ageWriter, file} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Extract(path, filepath.Join(base, "out"), identity, ""); err == nil {
		t.Fatal("symlink ancestor chain was accepted")
	}
	if _, err := os.Stat(filepath.Join(base, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("symlink chain wrote outside extraction root: %v", err)
	}
}

func TestExtractRejectsUnsignedPayloadAndUntrustedSigner(t *testing.T) {
	for _, test := range []string{"unsigned payload", "wrong signer"} {
		t.Run(test, func(t *testing.T) {
			base := t.TempDir()
			identity, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(base, "payload")
			if err := os.Mkdir(payload, 0o700); err != nil {
				t.Fatal(err)
			}
			manifest := snapshot.Manifest{SchemaVersion: snapshot.SchemaVersion, ProjectID: "test", PayloadHashes: map[string]string{}}
			for _, name := range []string{"repo.bundle", "index.patch", "worktree.patch"} {
				data := []byte(name)
				if err := os.WriteFile(filepath.Join(payload, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(data)
				manifest.PayloadHashes[name] = hex.EncodeToString(hash[:])
			}
			manifest.Git.BundleSHA256 = manifest.PayloadHashes["repo.bundle"]
			manifest.Git.IndexPatchSHA256 = manifest.PayloadHashes["index.patch"]
			manifest.Git.WorkPatchSHA256 = manifest.PayloadHashes["worktree.patch"]
			expectedSigner := base64.StdEncoding.EncodeToString(public)
			if test == "unsigned payload" {
				if err := os.WriteFile(filepath.Join(payload, "extra.txt"), []byte("not signed"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				other, _, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				expectedSigner = base64.StdEncoding.EncodeToString(other)
			}
			archive := filepath.Join(base, "snapshot.otg")
			if err := Create(payload, archive, manifest, identity.Recipient(), private); err != nil {
				t.Fatal(err)
			}
			if _, err := Extract(archive, filepath.Join(base, "out"), identity, expectedSigner); err == nil {
				t.Fatalf("accepted %s", test)
			}
		})
	}
}

func TestCreateChecksExpandedPayloadLimitBeforePublishing(t *testing.T) {
	base := t.TempDir()
	large, err := os.Create(filepath.Join(base, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(MaxPayloadSize + 1); err != nil {
		large.Close()
		t.Fatal(err)
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "snapshot.otg")
	if err := Create(base, output, snapshot.Manifest{}, identity.Recipient(), signer); err == nil {
		t.Fatal("oversized sparse payload was accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("oversized snapshot was published: %v", err)
	}
}

func TestExtractRejectsPathTraversalBeforeWrite(t *testing.T) {
	base := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "traversal.otg")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ageWriter, err := age.Encrypt(file, identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	zstdWriter, err := zstd.NewWriter(ageWriter)
	if err != nil {
		t.Fatal(err)
	}
	tarWriter := tar.NewWriter(zstdWriter)
	data := []byte("escaped")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "../escaped", Mode: 0o600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(data); err != nil {
		t.Fatal(err)
	}
	for _, closer := range []interface{ Close() error }{tarWriter, zstdWriter, ageWriter, file} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = Extract(path, filepath.Join(base, "out"), identity, "")
	var typed *otgerror.Error
	if !errors.As(err, &typed) || typed.Code != otgerror.CodePackageInvalid {
		t.Fatalf("expected E_PACKAGE_INVALID, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("path traversal wrote outside extraction root: %v", err)
	}
}
