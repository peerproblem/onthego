package restore

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/packageotg"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func secretArchivePath(name string) (string, error) {
	if len(name) != 68 || !strings.HasSuffix(name, ".age") {
		return "", fmt.Errorf("invalid encrypted backup locator")
	}
	if _, e := hex.DecodeString(strings.TrimSuffix(name, ".age")); e != nil {
		return "", e
	}
	home, e := config.DataRoot()
	if e != nil {
		return "", e
	}
	if e = fsx.CheckPath(home, "backups/"+name); e != nil {
		return "", e
	}
	return filepath.Join(home, "backups", name), nil
}

func saveSecretBackup(ctx context.Context, root string, b *backupState) error {
	count := 0
	for _, f := range b.Files {
		if f.Secret {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	keys, e := identity.LoadOrCreate()
	if e != nil {
		return e
	}
	recipient, e := keys.Recipient()
	if e != nil {
		return e
	}
	hash := sha256.Sum256([]byte(root + b.CreatedAt.String()))
	name := hex.EncodeToString(hash[:]) + ".age"
	target, e := secretArchivePath(name)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
		return e
	}
	if e = fsx.CheckPath(filepath.Dir(filepath.Dir(target)), "backups"); e != nil {
		return e
	}
	file, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(target)
		}
	}()
	encrypted, e := age.Encrypt(file, recipient)
	if e != nil {
		return e
	}
	writer := tar.NewWriter(encrypted)
	var total int64
	for i := range b.Files {
		f := &b.Files[i]
		if !f.Secret {
			continue
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if _, e = packageotg.ProjectPath(f.RelativePath); e != nil {
			return e
		}
		if e = packageotg.CheckParents(root, f.RelativePath); e != nil {
			return e
		}
		var data []byte
		if f.Kind == "symlink" {
			data = []byte(f.LinkTarget)
		} else {
			data, e = os.ReadFile(filepath.Join(root, filepath.FromSlash(f.RelativePath)))
			if e != nil {
				return e
			}
		}
		total += int64(len(data))
		if total > packageotg.MaxPayloadSize {
			return fmt.Errorf("secret backup exceeds payload limit")
		}
		digest := sha256.Sum256(data)
		f.SHA256 = hex.EncodeToString(digest[:])
		f.SizeBytes = int64(len(data))
		f.ArchivePath = "secret-files/" + f.RelativePath
		if e = writer.WriteHeader(&tar.Header{Name: f.ArchivePath, Mode: 0600, Size: int64(len(data))}); e != nil {
			return e
		}
		if _, e = writer.Write(data); e != nil {
			return e
		}
	}
	if e = writer.Close(); e != nil {
		return e
	}
	if e = encrypted.Close(); e != nil {
		return e
	}
	if e = file.Sync(); e != nil {
		return e
	}
	if e = file.Close(); e != nil {
		return e
	}
	b.SecretSHA256, _, e = fsx.HashRegular(target)
	if e != nil {
		return e
	}
	b.SecretArchive = name
	complete = true
	return nil
}

func loadSecretBackup(ctx context.Context, b backupState) (string, error) {
	files := map[string]snapshot.File{}
	for _, f := range b.Files {
		if f.Secret {
			files[f.ArchivePath] = f
		}
	}
	if len(files) == 0 {
		if b.SecretArchive != "" {
			return "", fmt.Errorf("unexpected secret backup artifact")
		}
		return "", nil
	}
	path, e := secretArchivePath(b.SecretArchive)
	if e != nil {
		return "", e
	}
	digest, _, e := fsx.HashRegular(path)
	if e != nil {
		return "", e
	}
	if digest != b.SecretSHA256 {
		return "", fmt.Errorf("encrypted backup digest mismatch")
	}
	keys, e := identity.LoadOrCreate()
	if e != nil {
		return "", e
	}
	secret, e := keys.Identity()
	if e != nil {
		return "", e
	}
	input, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer input.Close()
	decrypted, e := age.Decrypt(input, secret)
	if e != nil {
		return "", e
	}
	temp, e := tempDir("undo-secrets-")
	if e != nil {
		return "", e
	}
	complete := false
	defer func() {
		if !complete {
			fsx.ReleaseTemp(temp)
		}
	}()
	reader := tar.NewReader(decrypted)
	seen := map[string]bool{}
	var total int64
	for {
		if e = ctx.Err(); e != nil {
			return "", e
		}
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		f, ok := files[header.Name]
		if !ok || seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size != f.SizeBytes {
			return "", fmt.Errorf("invalid encrypted backup entry")
		}
		seen[header.Name] = true
		total += header.Size
		if total > packageotg.MaxPayloadSize {
			return "", fmt.Errorf("encrypted backup exceeds payload limit")
		}
		if _, e = packageotg.ProjectPath(f.RelativePath); e != nil {
			return "", e
		}
		if header.Name != "secret-files/"+f.RelativePath {
			return "", fmt.Errorf("invalid backup secret path")
		}
		target := filepath.Join(temp, filepath.FromSlash(header.Name))
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return "", e
		}
		file, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return "", e
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(file, hash), reader)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != f.SHA256 {
			return "", fmt.Errorf("backup secret content mismatch")
		}
	}
	if len(seen) != len(files) {
		return "", fmt.Errorf("encrypted backup entries missing")
	}
	if _, e = io.Copy(io.Discard, decrypted); e != nil {
		return "", e
	}
	complete = true
	return temp, nil
}

func removeSecretBackup(b backupState) error {
	if b.SecretArchive == "" {
		return nil
	}
	path, e := secretArchivePath(b.SecretArchive)
	if e != nil {
		return e
	}
	e = os.Remove(path)
	if os.IsNotExist(e) {
		return nil
	}
	return e
}
