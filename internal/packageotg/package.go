package packageotg

import (
	"archive/tar"
	"bufio"
	"context"
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
	"github.com/klauspost/compress/zstd"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/snapshot"
)

const (
	MaxFiles        = 20000
	MaxManifestSize = int64(8 << 20)
	MaxBundleSize   = int64(64 << 30) // Independent transport safety limit, not workspace payload.
)

// MaxPayloadSize is shared by packing and extraction. Tests may lower it.
var MaxPayloadSize = int64(250 << 20)

func Create(payloadDir, output string, manifest snapshot.Manifest, recipient age.Recipient, signer ed25519.PrivateKey) error {
	return CreateContext(context.Background(), payloadDir, output, manifest, recipient, signer)
}

// CreateContext checks cancellation while reading, compressing and encrypting
// payload bytes. A canceled package is never published to output.
func CreateContext(ctx context.Context, payloadDir, output string, manifest snapshot.Manifest, recipient age.Recipient, signer ed25519.PrivateKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(signer) != ed25519.PrivateKeySize {
		return errors.New("invalid Ed25519 signing key")
	}
	sizes, err := PayloadSizesContext(ctx, payloadDir)
	if err != nil {
		return err
	}
	if manifest.SchemaVersion >= 3 {
		if manifest.PayloadSizes == nil {
			manifest.PayloadSizes = sizes
		} else if !equalPayloadSizes(manifest.PayloadSizes, sizes) {
			return packageError("payload sizes changed before packing", nil)
		}
	}
	if len(sizes) != len(manifest.PayloadHashes) {
		return packageError("payload files do not match signed manifest", nil)
	}
	for path := range sizes {
		if manifest.PayloadHashes[path] == "" {
			return packageError("payload file is not covered by manifest: "+path, nil)
		}
	}
	if err := validateManifest(manifest); err != nil {
		return packageError("manifest paths or payload metadata are invalid", err)
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
	if int64(len(manifestJSON)) > MaxManifestSize {
		return packageError("manifest exceeds 8 MiB limit", nil)
	}
	if err := checkPayloadLimitsContext(ctx, payloadDir, int64(len(manifestJSON))+int64(base64.StdEncoding.EncodedLen(ed25519.SignatureSize))); err != nil {
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
	ageWriter, err := age.Encrypt(contextWriter{ctx, tmp}, recipient)
	if err != nil {
		tmp.Close()
		return err
	}
	zstdWriter, err := zstd.NewWriter(ageWriter, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return err
	}
	defer zstdWriter.Close()
	tarWriter := tar.NewWriter(contextWriter{ctx, zstdWriter})
	if err := writeBytes(tarWriter, "manifest.json", manifestJSON, 0o600); err != nil {
		return err
	}
	if err := writeBytes(tarWriter, "manifest.sig", []byte(base64.StdEncoding.EncodeToString(signature)), 0o600); err != nil {
		return err
	}
	if err := writeTreeContext(ctx, tarWriter, payloadDir); err != nil {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	// Linking publishes the complete package without replacing an existing one.
	return os.Link(name, output)
}

func Extract(packagePath, outputDir string, ageIdentity age.Identity, expectedSigner string) (snapshot.Manifest, error) {
	return ExtractContext(context.Background(), packagePath, outputDir, ageIdentity, expectedSigner)
}

// ExtractContext checks cancellation through encrypted, compressed and decoded
// reads, including signature-bound payload hashing. The caller owns outputDir
// and must remove its temporary extraction directory on any failure.
func ExtractContext(ctx context.Context, packagePath, outputDir string, ageIdentity age.Identity, expectedSigner string) (snapshot.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return snapshot.Manifest{}, err
	}
	input, err := os.Open(packagePath)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	defer input.Close()
	decrypted, err := age.Decrypt(bufio.NewReader(contextReader{ctx, input}), ageIdentity)
	if err != nil {
		return snapshot.Manifest{}, packageError("age decryption failed", err)
	}
	zstdReader, err := zstd.NewReader(contextReader{ctx, decrypted}, zstd.WithDecoderMaxMemory(uint64(500<<20)), zstd.WithDecoderConcurrency(1))
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
	tarReader := tar.NewReader(contextReader{ctx, zstdReader})
	manifestData, err := readManifestEntry(tarReader, "manifest.json")
	if err != nil {
		return snapshot.Manifest{}, err
	}
	signatureData, err := readManifestEntry(tarReader, "manifest.sig")
	if err != nil {
		return snapshot.Manifest{}, err
	}
	manifest, err := verifyManifest(manifestData, signatureData, expectedSigner)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	total := int64(len(manifestData) + len(signatureData))
	if total > MaxPayloadSize {
		return snapshot.Manifest{}, packageError("archive extraction limit exceeded", nil)
	}
	for name, data := range map[string][]byte{"manifest.json": manifestData, "manifest.sig": signatureData} {
		file, err := os.OpenFile(filepath.Join(outputDir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return snapshot.Manifest{}, err
		}
		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil {
			return snapshot.Manifest{}, writeErr
		}
		if closeErr != nil {
			return snapshot.Manifest{}, closeErr
		}
	}
	// Older packages contain explicit directory entries, including empty ones.
	// Schema 2 directories are ignored without materializing them. Schema 3
	// directory names must derive from authenticated payload paths.
	directories := map[string]bool{}
	for name := range manifest.PayloadHashes {
		for parent := filepath.ToSlash(filepath.Dir(name)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			directories[parent] = true
		}
	}
	seen := map[string]bool{"manifest.json": true, "manifest.sig": true}
	count := 2
	for {
		if err := ctx.Err(); err != nil {
			return snapshot.Manifest{}, err
		}
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
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 || (manifest.SchemaVersion >= 3 && !directories[name]) {
				return snapshot.Manifest{}, packageError("archive directory is not covered by signed payloads", nil)
			}
			// Regular file extraction creates authenticated parent directories.
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return snapshot.Manifest{}, packageError("archive contains unsupported entry type", nil)
		}
		if manifest.PayloadHashes[name] == "" {
			return snapshot.Manifest{}, packageError("archive contains unsigned payload: "+name, nil)
		}
		if manifest.SchemaVersion >= 3 && header.Size != manifest.PayloadSizes[name] {
			return snapshot.Manifest{}, packageError("archive entry size does not match signed manifest: "+name, nil)
		}
		if header.Size < 0 {
			return snapshot.Manifest{}, packageError("archive entry has negative size", nil)
		}
		if name == "repo.bundle" {
			if header.Size > MaxBundleSize {
				return snapshot.Manifest{}, packageError("Git bundle exceeds transport safety limit", nil)
			}
		} else {
			if header.Size > MaxPayloadSize-total {
				return snapshot.Manifest{}, packageError("archive extraction limit exceeded", nil)
			}
			total += header.Size
		}
		destination := filepath.Join(outputDir, filepath.FromSlash(name))
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
		_, copyErr := io.CopyN(contextWriter{ctx, file}, contextReader{ctx, tarReader}, header.Size)
		closeErr := file.Close()
		if copyErr != nil {
			return snapshot.Manifest{}, packageError("archive entry is truncated", copyErr)
		}
		if closeErr != nil {
			return snapshot.Manifest{}, closeErr
		}
	}
	return verifyPayloadContext(ctx, outputDir, manifest)
}

func readManifestEntry(reader *tar.Reader, name string) ([]byte, error) {
	header, err := reader.Next()
	if err != nil {
		return nil, packageError("manifest and signature must be the first two archive entries", err)
	}
	if header.Name != name || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > MaxManifestSize {
		return nil, packageError("expected bounded regular archive entry: "+name, nil)
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxManifestSize+1))
	if err != nil || int64(len(data)) != header.Size {
		return nil, packageError("manifest entry is truncated", err)
	}
	return data, nil
}

func verifyExtracted(root, expectedSigner string) (snapshot.Manifest, error) {
	return verifyExtractedContext(context.Background(), root, expectedSigner)
}

func verifyExtractedContext(ctx context.Context, root, expectedSigner string) (snapshot.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return snapshot.Manifest{}, err
	}
	manifestData, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return snapshot.Manifest{}, packageError("manifest missing", err)
	}
	signatureText, err := os.ReadFile(filepath.Join(root, "manifest.sig"))
	if err != nil {
		return snapshot.Manifest{}, packageError("manifest signature missing", err)
	}
	manifest, err := verifyManifest(manifestData, signatureText, expectedSigner)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	return verifyPayloadContext(ctx, root, manifest)
}

func verifyManifest(manifestData, signatureText []byte, expectedSigner string) (snapshot.Manifest, error) {
	if int64(len(manifestData)) > MaxManifestSize || int64(len(signatureText)) > MaxManifestSize {
		return snapshot.Manifest{}, packageError("manifest entry exceeds 8 MiB limit", nil)
	}
	var manifest snapshot.Manifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestData)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return snapshot.Manifest{}, packageError("manifest invalid", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return snapshot.Manifest{}, packageError("manifest contains trailing JSON", err)
	}
	if expectedSigner != "" && manifest.SignerPublic != expectedSigner {
		return snapshot.Manifest{}, packageError("package signer is not trusted", nil)
	}
	publicKey, err := identity.ParsePublic(manifest.SignerPublic)
	if err != nil {
		return snapshot.Manifest{}, packageError("signer key is invalid", err)
	}
	signature, err := base64.StdEncoding.DecodeString(string(signatureText))
	if err != nil {
		return snapshot.Manifest{}, packageError("manifest signature invalid", err)
	}
	canonical, err := manifest.Canonical()
	if err != nil || !ed25519.Verify(publicKey, canonical, signature) {
		return snapshot.Manifest{}, packageError("manifest signature verification failed", err)
	}
	if err := validateManifest(manifest); err != nil {
		return snapshot.Manifest{}, packageError("manifest paths or payload metadata are invalid", err)
	}
	if err := manifest.ValidateID(); err != nil {
		return snapshot.Manifest{}, packageError(err.Error(), err)
	}
	return manifest, nil
}

func verifyPayloadContext(ctx context.Context, root string, manifest snapshot.Manifest) (snapshot.Manifest, error) {
	for path, expected := range manifest.PayloadHashes {
		if err := verifyFileHashContext(ctx, root, path, expected); err != nil {
			return snapshot.Manifest{}, err
		}
	}
	// Every regular payload must be covered by the signed manifest. Otherwise an
	// attacker could add context or executable files beside authenticated ones.
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
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
			return fmt.Errorf("unexpected symlink payload: %s", rel)
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
	if (manifest.SchemaVersion != 2 && manifest.SchemaVersion != snapshot.SchemaVersion) || manifest.ProjectID == "" {
		return errors.New("manifest schema is unsupported")
	}
	if len(manifest.PayloadHashes) > MaxFiles-2 || len(manifest.Files) > MaxFiles || len(manifest.RetainedPaths) > MaxFiles {
		return errors.New("manifest contains too many paths")
	}
	if manifest.SchemaVersion == 2 && (len(manifest.PayloadSizes) != 0 || len(manifest.RetainedPaths) != 0) {
		return errors.New("schema 2 cannot contain schema 3 metadata")
	}
	if manifest.SchemaVersion >= 3 {
		if len(manifest.PayloadSizes) != len(manifest.PayloadHashes) {
			return errors.New("payload sizes must cover every signed payload")
		}
		for path, size := range manifest.PayloadSizes {
			if manifest.PayloadHashes[path] == "" || size < 0 {
				return fmt.Errorf("invalid signed payload size: %s", path)
			}
			if path == "repo.bundle" && size > MaxBundleSize {
				return errors.New("Git bundle exceeds transport safety limit")
			}
		}
	}
	if !validHash(manifest.Git.BundleSHA256) || manifest.PayloadHashes["repo.bundle"] != manifest.Git.BundleSHA256 {
		return fmt.Errorf("missing or inconsistent repo.bundle hash")
	}
	for _, id := range []string{manifest.SourceGitHead, manifest.Git.WIPCommit, manifest.Git.IndexTree, manifest.Git.WorktreeTree, manifest.Git.UntrackedTree} {
		if !gitx.ValidOID(id) {
			return fmt.Errorf("invalid Git object ID")
		}
	}
	if len(manifest.ParentSnapshotIDs) > 1 {
		return fmt.Errorf("snapshot must have at most one direct parent")
	}
	for path, hash := range manifest.PayloadHashes {
		if _, err := safeArchivePath(path); err != nil || !validHash(hash) || path == "manifest.json" || path == "manifest.sig" {
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
		if !validHash(file.SHA256) || file.SizeBytes < 0 {
			return fmt.Errorf("invalid selected file metadata: %s", rel)
		}
		if file.Secret {
			if file.ArchivePath != "secret-files/"+rel || manifest.PayloadHashes[file.ArchivePath] != file.SHA256 {
				return fmt.Errorf("invalid encrypted secret payload: %s", rel)
			}
		} else if file.ArchivePath != "" {
			return fmt.Errorf("nonsecret file must use the Git tree: %s", rel)
		}
		switch file.Kind {
		case "file":

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
	retained := map[string]bool{}
	for _, path := range manifest.RetainedPaths {
		rel, err := ProjectPath(path)
		if err != nil || retained[rel] || seen[rel] {
			return fmt.Errorf("unsafe, duplicate, or selected retained path: %s", path)
		}
		retained[rel] = true
	}
	return nil
}

func validHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

// PayloadSizesContext records the exact regular-file sizes that schema 3 signs.
// Capture calls this before assigning the snapshot ID; Create checks it again.
func PayloadSizesContext(ctx context.Context, root string) (map[string]int64, error) {
	sizes := map[string]int64{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return packageError("payload must be a regular file", nil)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name, err := safeArchivePath(filepath.ToSlash(rel))
		if err != nil || name == "manifest.json" || name == "manifest.sig" {
			return packageError("payload contains reserved or unsafe path", err)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		sizes[name] = info.Size()
		return nil
	})
	return sizes, err
}

func equalPayloadSizes(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for name, size := range left {
		if actual, ok := right[name]; !ok || actual != size {
			return false
		}
	}
	return true
}

func checkPayloadLimits(root string, manifestBytes int64) error {
	return checkPayloadLimitsContext(context.Background(), root, manifestBytes)
}

func checkPayloadLimitsContext(ctx context.Context, root string, manifestBytes int64) error {
	total, count := manifestBytes, 2 // manifest.json and manifest.sig
	if total > MaxPayloadSize {
		return packageError("snapshot exceeds expanded payload limit", nil)
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
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
			if filepath.Base(path) == "repo.bundle" && filepath.Dir(path) == root {
				if info.Size() > MaxBundleSize {
					return packageError("Git bundle exceeds transport safety limit", nil)
				}
				return nil
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
	return writeTreeContext(context.Background(), writer, root)
}

func writeTreeContext(ctx context.Context, writer *tar.Writer, root string) error {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path != root && !entry.IsDir() {
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
			_, copyErr := io.Copy(writer, contextReader{ctx, file})
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
	return verifyFileHashContext(context.Background(), root, archivePath, expected)
}

func verifyFileHashContext(ctx context.Context, root, archivePath, expected string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if _, err := io.Copy(hash, contextReader{ctx, file}); err != nil {
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

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err == nil {
		err = r.ctx.Err()
	}
	return n, err
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(p)
	if err == nil {
		err = w.ctx.Err()
	}
	return n, err
}
