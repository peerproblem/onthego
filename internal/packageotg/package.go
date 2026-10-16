package packageotg

import (
	"archive/tar"
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jbaehova/onthego/internal/identity"
	"github.com/jbaehova/onthego/internal/otgerror"
	"github.com/jbaehova/onthego/internal/snapshot"
	"github.com/klauspost/compress/zstd"
)

const (
	MaxFiles       = 20000
	MaxPayloadSize = int64(250 << 20)
	MaxRatio       = int64(200)
)

func Create(payloadDir, output string, manifest snapshot.Manifest, recipient age.Recipient, signer ed25519.PrivateKey) error {
	if len(signer) != ed25519.PrivateKeySize {
		return errors.New("invalid Ed25519 signing key")
	}
	manifest.SignerPublic = base64.StdEncoding.EncodeToString(signer.Public().(ed25519.PublicKey))
	if err := manifest.SetSnapshotID(); err != nil {
		return err
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		return err
	}
	signature := ed25519.Sign(signer, canonical)
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := checkPayloadLimits(payloadDir, int64(len(manifestJSON))+int64(base64.StdEncoding.EncodedLen(ed25519.SignatureSize))); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".onthego-package-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	ageWriter, err := age.Encrypt(tmp, recipient)
	if err != nil {
		tmp.Close()
		return err
	}
	zstdWriter, err := zstd.NewWriter(ageWriter, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return err
	}
	tarWriter := tar.NewWriter(zstdWriter)
	if err := writeBytes(tarWriter, "manifest.json", manifestJSON, 0o600); err != nil {
		return err
	}
	if err := writeBytes(tarWriter, "manifest.sig", []byte(base64.StdEncoding.EncodeToString(signature)), 0o600); err != nil {
		return err
	}
	if err := writeTree(tarWriter, payloadDir); err != nil {
		return err
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := zstdWriter.Close(); err != nil {
		return err
	}
	if err := ageWriter.Close(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Linking publishes the complete package without replacing an existing one.
	return os.Link(name, output)
}

func Extract(packagePath, outputDir string, ageIdentity age.Identity, expectedSigner string) (snapshot.Manifest, error) {
	packageInfo, err := os.Stat(packagePath)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	input, err := os.Open(packagePath)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	defer input.Close()
	decrypted, err := age.Decrypt(bufio.NewReader(input), ageIdentity)
	if err != nil {
		return snapshot.Manifest{}, packageError("age decryption failed", err)
	}
	zstdReader, err := zstd.NewReader(decrypted, zstd.WithDecoderMaxMemory(uint64(MaxPayloadSize*2)))
	if err != nil {
		return snapshot.Manifest{}, packageError("zstd stream is invalid", err)
	}
	defer zstdReader.Close()
	if info, err := os.Lstat(outputDir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return snapshot.Manifest{}, packageError("extraction root must be a real directory", nil)
	} else if err != nil && !os.IsNotExist(err) {
		return snapshot.Manifest{}, err
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return snapshot.Manifest{}, err
	}
	tarReader := tar.NewReader(zstdReader)
	seen := map[string]bool{}
	var total int64
	var count int
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return snapshot.Manifest{}, packageError("tar stream is invalid", err)
		}
		count++
		if count > MaxFiles {
			return snapshot.Manifest{}, packageError("archive contains too many entries", nil)
		}
		name, err := safeArchivePath(header.Name)
		if err != nil || seen[name] {
			return snapshot.Manifest{}, packageError("archive contains an unsafe or duplicate path", err)
		}
		seen[name] = true
		if err := CheckParents(outputDir, name); err != nil {
			return snapshot.Manifest{}, packageError("archive path traverses a symlink", err)
		}
		total += header.Size
		if total > MaxPayloadSize || (packageInfo.Size() > 0 && total > packageInfo.Size()*MaxRatio) {
			return snapshot.Manifest{}, packageError("archive extraction limit exceeded", nil)
		}
		destination := filepath.Join(outputDir, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if info, err := os.Lstat(destination); err == nil && !info.IsDir() {
				return snapshot.Manifest{}, packageError("archive directory conflicts with an existing file", nil)
			}
			if err := os.MkdirAll(destination, 0o700); err != nil {
				return snapshot.Manifest{}, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return snapshot.Manifest{}, err
			}
			mode := fs.FileMode(header.Mode) & 0o777
			if mode == 0 {
				mode = 0o600
			}
			file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return snapshot.Manifest{}, err
			}
			_, copyErr := io.CopyN(file, tarReader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return snapshot.Manifest{}, packageError("archive entry is truncated", copyErr)
			}
			if closeErr != nil {
				return snapshot.Manifest{}, closeErr
			}
		case tar.TypeSymlink:
			if !strings.HasPrefix(name, "files/") {
				return snapshot.Manifest{}, packageError("symlinks are allowed only in selected worktree files", nil)
			}
			link, err := safeLinkTarget(name, header.Linkname)
			if err != nil {
				return snapshot.Manifest{}, packageError("archive contains unsafe symlink", err)
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return snapshot.Manifest{}, err
			}
			if err := os.Symlink(link, destination); err != nil {
				return snapshot.Manifest{}, err
			}
		default:
			return snapshot.Manifest{}, packageError("archive contains unsupported entry type", nil)
		}
	}
	return verifyExtracted(outputDir, expectedSigner)
}

func verifyExtracted(root, expectedSigner string) (snapshot.Manifest, error) {
	manifestData, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return snapshot.Manifest{}, packageError("manifest missing", err)
	}
	var manifest snapshot.Manifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestData)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return snapshot.Manifest{}, packageError("manifest invalid", err)
	}
	if manifest.SchemaVersion != snapshot.SchemaVersion || manifest.ProjectID == "" {
		return snapshot.Manifest{}, packageError("manifest schema is unsupported", nil)
	}
	if err := validateManifest(manifest); err != nil {
		return snapshot.Manifest{}, packageError("manifest paths or payload metadata are invalid", err)
	}
	if err := manifest.ValidateID(); err != nil {
		return snapshot.Manifest{}, packageError(err.Error(), err)
	}
	if expectedSigner != "" && manifest.SignerPublic != expectedSigner {
		return snapshot.Manifest{}, packageError("package signer is not trusted", nil)
	}
	publicKey, err := identity.ParsePublic(manifest.SignerPublic)
	if err != nil {
		return snapshot.Manifest{}, packageError("signer key is invalid", err)
	}
	signatureText, err := os.ReadFile(filepath.Join(root, "manifest.sig"))
	if err != nil {
		return snapshot.Manifest{}, packageError("manifest signature missing", err)
	}
	signature, err := base64.StdEncoding.DecodeString(string(signatureText))
	if err != nil {
		return snapshot.Manifest{}, packageError("manifest signature invalid", err)
	}
	canonical, err := manifest.Canonical()
	if err != nil || !ed25519.Verify(publicKey, canonical, signature) {
		return snapshot.Manifest{}, packageError("manifest signature verification failed", err)
	}
	for path, expected := range manifest.PayloadHashes {
		if err := verifyFileHash(root, path, expected); err != nil {
			return snapshot.Manifest{}, err
		}
	}
	for _, file := range manifest.Files {
		if file.Kind == "tombstone" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(file.ArchivePath))
		if file.Kind == "symlink" {
			target, err := os.Readlink(path)
			if err != nil || target != file.LinkTarget {
				return snapshot.Manifest{}, packageError("symlink payload mismatch", err)
			}
			continue
		}
		if err := verifyFileHash(root, file.ArchivePath, file.SHA256); err != nil {
			return snapshot.Manifest{}, err
		}
	}
	// Every regular payload must be covered by the signed manifest. Otherwise an
	// attacker could add context or executable files beside authenticated ones.
	symlinks := map[string]bool{}
	for _, file := range manifest.Files {
		if file.Kind == "symlink" {
			symlinks[file.ArchivePath] = true
		}
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			if !symlinks[rel] {
				return fmt.Errorf("unsigned symlink: %s", rel)
			}
			return nil
		}
		if rel != "manifest.json" && rel != "manifest.sig" && manifest.PayloadHashes[rel] == "" {
			return fmt.Errorf("unsigned payload: %s", rel)
		}
		return nil
	}); err != nil {
		return snapshot.Manifest{}, packageError("package contains unauthenticated payload", err)
	}
	return manifest, nil
}

func validateManifest(manifest snapshot.Manifest) error {
	for path, hash := range map[string]string{
		"repo.bundle":    manifest.Git.BundleSHA256,
		"index.patch":    manifest.Git.IndexPatchSHA256,
		"worktree.patch": manifest.Git.WorkPatchSHA256,
	} {
		if !validHash(hash) || manifest.PayloadHashes[path] != hash {
			return fmt.Errorf("missing or inconsistent %s hash", path)
		}
	}
	for path, hash := range manifest.PayloadHashes {
		if _, err := safeArchivePath(path); err != nil || !validHash(hash) {
			return fmt.Errorf("invalid payload path or hash: %s", path)
		}
	}
	seen := map[string]bool{}
	for _, file := range manifest.Files {
		rel, err := ProjectPath(file.RelativePath)
		if err != nil || seen[rel] {
			return fmt.Errorf("unsafe or duplicate selected path: %s", file.RelativePath)
		}
		seen[rel] = true
		if file.Kind == "tombstone" {
			continue
		}
		if file.ArchivePath != "files/"+rel || !validHash(file.SHA256) {
			return fmt.Errorf("invalid selected payload: %s", rel)
		}
		switch file.Kind {
		case "file":
			if manifest.PayloadHashes[file.ArchivePath] != file.SHA256 {
				return fmt.Errorf("selected payload hash is inconsistent: %s", rel)
			}
		case "symlink":
			if _, err := safeLinkTarget(rel, file.LinkTarget); err != nil {
				return err
			}
			hash := sha256.Sum256([]byte(file.LinkTarget))
			if hex.EncodeToString(hash[:]) != file.SHA256 {
				return fmt.Errorf("selected symlink hash is inconsistent: %s", rel)
			}
		default:
			return fmt.Errorf("unsupported selected file kind: %s", file.Kind)
		}
	}
	return nil
}

func validHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func checkPayloadLimits(root string, manifestBytes int64) error {
	total, count := manifestBytes, 2 // manifest.json and manifest.sig
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		count++
		if count > MaxFiles {
			return packageError("snapshot contains too many files; exclude generated directories", nil)
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			if total > MaxPayloadSize {
				return packageError("snapshot exceeds the 250 MiB expanded payload limit", nil)
			}
		}
		return nil
	})
}

func writeTree(writer *tar.Writer, root string) error {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(paths, func(i, j int) bool {
		left, _ := filepath.Rel(root, paths[i])
		right, _ := filepath.Rel(root, paths[j])
		return filepath.ToSlash(left) < filepath.ToSlash(right)
	})
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name, err := safeArchivePath(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		header.ModTime = time.Unix(0, 0).UTC()
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.Format = tar.FormatPAX
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = target
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(writer, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return nil
}

func writeBytes(writer *tar.Writer, name string, data []byte, mode int64) error {
	header := &tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func safeArchivePath(name string) (string, error) {
	name = filepath.ToSlash(name)
	clean := filepath.ToSlash(filepath.Clean(name))
	if name == "" || name == "." || clean != name || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") || (len(name) > 1 && name[1] == ':') {
		return "", errors.New("unsafe archive path")
	}
	return name, nil
}

func safeLinkTarget(entry, target string) (string, error) {
	if target == "" || filepath.IsAbs(target) || strings.ContainsAny(target, "\\\x00") || (len(target) > 1 && target[1] == ':') {
		return "", errors.New("absolute symlink target")
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(entry)), target))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return "", errors.New("symlink escapes archive root")
	}
	return target, nil
}

func verifyFileHash(root, archivePath, expected string) error {
	path, err := safeArchivePath(archivePath)
	if err != nil {
		return packageError("manifest contains unsafe payload path", err)
	}
	if err := CheckParents(root, path); err != nil {
		return packageError("payload path traverses a symlink", err)
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil || !info.Mode().IsRegular() {
		return packageError("payload is not a regular file", err)
	}
	file, err := os.Open(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return packageError("payload file missing", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return packageError(fmt.Sprintf("payload hash mismatch for %s", archivePath), nil)
	}
	return nil
}

func packageError(message string, err error) error {
	return &otgerror.Error{Code: otgerror.CodePackageInvalid, Message: message, Cause: err}
}
