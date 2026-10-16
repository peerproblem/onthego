package capture

import (
	"bytes"
	"context"
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
	"github.com/jbaehova/onthego/internal/config"
	"github.com/jbaehova/onthego/internal/gitx"
	"github.com/jbaehova/onthego/internal/identity"
	"github.com/jbaehova/onthego/internal/otgerror"
	"github.com/jbaehova/onthego/internal/packageotg"
	"github.com/jbaehova/onthego/internal/snapshot"
)

type Options struct {
	Root              string
	Config            config.Config
	Output            string
	ContextDir        string
	Include           []string
	IncludeSecret     []string
	ParentSnapshotID  string
	ParentSnapshotIDs []string
	EnvironmentID     string
	OnthegoVersion    string
	Recipient         age.Recipient
	Keys              identity.Keys
}

type Result struct {
	Manifest snapshot.Manifest `json:"manifest"`
	Output   string            `json:"output"`
}

type PreviewFile struct {
	RelativePath string `json:"relative_path"`
	Secret       bool   `json:"secret"`
	Policy       string `json:"policy"`
}

func Preview(ctx context.Context, options Options) ([]PreviewFile, error) {
	if err := gitx.Validate(ctx, options.Root); err != nil {
		return nil, err
	}
	selected, err := selectFiles(ctx, options)
	if err != nil {
		return nil, err
	}
	result := make([]PreviewFile, 0, len(selected))
	for _, file := range selected {
		result = append(result, PreviewFile{RelativePath: file.Relative, Secret: file.Secret, Policy: file.Policy})
	}
	return result, nil
}

type selectedFile struct {
	Relative string
	Secret   bool
	Policy   string
}

func Create(ctx context.Context, options Options) (Result, error) {
	if err := gitx.Validate(ctx, options.Root); err != nil {
		return Result{}, err
	}
	if options.Output == "" {
		return Result{}, otgerror.New(otgerror.CodeInput, "snapshot output path is required")
	}
	if options.EnvironmentID == "" {
		options.EnvironmentID = "local"
	}
	if options.Recipient == nil {
		var err error
		options.Recipient, err = options.Keys.Recipient()
		if err != nil {
			return Result{}, err
		}
	}
	signer, err := options.Keys.PrivateKey()
	if err != nil {
		return Result{}, err
	}
	dataRoot, err := config.ProjectDataRoot(options.Config.ProjectID)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return Result{}, err
	}
	unlock, err := acquireLock(filepath.Join(dataRoot, "capture.lock"))
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	before, err := Fingerprint(ctx, options)
	if err != nil {
		return Result{}, err
	}
	selected, err := selectFiles(ctx, options)
	if err != nil {
		return Result{}, err
	}
	payload, err := os.MkdirTemp(dataRoot, "capture-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(payload)

	head, err := gitText(ctx, options.Root, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	branch, err := gitText(ctx, options.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		branch = "(detached)"
	}
	bundlePath := filepath.Join(payload, "repo.bundle")
	if _, err := gitx.Run(ctx, options.Root, "bundle", "create", bundlePath, "--all", "HEAD"); err != nil {
		return Result{}, err
	}
	indexPatch, err := gitx.Run(ctx, options.Root, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return Result{}, err
	}
	workPatch, err := gitx.Run(ctx, options.Root, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(payload, "index.patch"), indexPatch.Stdout, 0o600); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(payload, "worktree.patch"), workPatch.Stdout, 0o600); err != nil {
		return Result{}, err
	}

	manifest := snapshot.Manifest{
		SchemaVersion:       snapshot.SchemaVersion,
		ProjectID:           options.Config.ProjectID,
		SourceEnvironmentID: options.EnvironmentID,
		SourceGitHead:       head,
		SourceBranch:        branch,
		CreatedAt:           time.Now().UTC().Truncate(time.Second),
		PayloadHashes:       map[string]string{},
		Toolchain: snapshot.Toolchain{
			Onthego: options.OnthegoVersion,
		},
		SignerPublic: options.Keys.SigningPublic,
	}
	parents := map[string]bool{}
	for _, id := range append(append([]string(nil), options.ParentSnapshotIDs...), options.ParentSnapshotID) {
		if id != "" && !parents[id] {
			manifest.ParentSnapshotIDs = append(manifest.ParentSnapshotIDs, id)
			parents[id] = true
		}
	}
	sort.Strings(manifest.ParentSnapshotIDs)
	manifest.Toolchain.GitVersion, _ = gitx.Version(ctx)
	manifest.Git.Ref = branch
	if origin, err := gitText(ctx, options.Root, "remote", "get-url", "origin"); err == nil {
		manifest.Git.OriginURL = gitx.SafeOriginURL(origin)
	}
	for _, item := range []struct {
		name string
		path string
	}{
		{"repo.bundle", bundlePath},
		{"index.patch", filepath.Join(payload, "index.patch")},
		{"worktree.patch", filepath.Join(payload, "worktree.patch")},
	} {
		hash, _, err := hashRegular(item.path)
		if err != nil {
			return Result{}, err
		}
		manifest.PayloadHashes[item.name] = hash
		switch item.name {
		case "repo.bundle":
			manifest.Git.BundleSHA256 = hash
		case "index.patch":
			manifest.Git.IndexPatchSHA256 = hash
		case "worktree.patch":
			manifest.Git.WorkPatchSHA256 = hash
		}
	}

	for _, file := range selected {
		entry, err := copySelected(options.Root, payload, file)
		if err != nil {
			return Result{}, err
		}
		manifest.Files = append(manifest.Files, entry)
		if entry.Kind == "file" {
			manifest.PayloadHashes[entry.ArchivePath] = entry.SHA256
		}
	}
	if options.ContextDir != "" {
		if err := copyContext(options.ContextDir, payload, &manifest); err != nil {
			return Result{}, err
		}
	}
	if err := manifest.SetSnapshotID(); err != nil {
		return Result{}, err
	}
	after, err := Fingerprint(ctx, options)
	if err != nil {
		return Result{}, err
	}
	if before != after {
		return Result{}, &otgerror.Error{Code: otgerror.CodeSourceChanged, Message: "project changed while the snapshot was being captured", Retryable: true, Stage: "capture"}
	}
	if err := os.MkdirAll(filepath.Dir(options.Output), 0o700); err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(options.Output); err == nil {
		return Result{}, otgerror.New(otgerror.CodeInput, "snapshot output already exists")
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	if err := packageotg.Create(payload, options.Output, manifest, options.Recipient, signer); err != nil {
		return Result{}, err
	}
	return Result{Manifest: manifest, Output: options.Output}, nil
}

func selectFiles(ctx context.Context, options Options) ([]selectedFile, error) {
	chosen := map[string]selectedFile{}
	untracked, err := gitx.Run(ctx, options.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, raw := range bytes.Split(untracked.Stdout, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		rel, err := config.ValidateRelative(options.Root, string(raw))
		if err != nil || excluded(rel, options.Config.Exclude) {
			continue
		}
		if isSecretCandidate(rel) || alwaysDenied(rel) {
			continue
		}
		chosen[rel] = selectedFile{Relative: rel, Policy: "untracked"}
	}
	for _, pattern := range append(append([]string(nil), options.Config.Include...), options.Include...) {
		paths, err := expand(options.Root, pattern)
		if err != nil {
			return nil, err
		}
		for _, rel := range paths {
			if excluded(rel, options.Config.Exclude) {
				continue
			}
			if alwaysDenied(rel) {
				return nil, otgerror.New(otgerror.CodeSecretPolicy, "authentication files and private keys cannot be included: "+rel)
			}
			if isSecretCandidate(rel) {
				return nil, otgerror.New(otgerror.CodeSecretPolicy, "secret candidate requires --include-secret: "+rel)
			}
			chosen[rel] = selectedFile{Relative: rel, Policy: "include"}
		}
	}
	for _, raw := range options.IncludeSecret {
		rel, err := config.ValidateRelative(options.Root, raw)
		if err != nil {
			return nil, otgerror.Wrap(otgerror.CodeSecretPolicy, err.Error(), err)
		}
		if alwaysDenied(rel) {
			return nil, otgerror.New(otgerror.CodeSecretPolicy, "authentication files and private keys cannot be included: "+rel)
		}
		if excluded(rel, options.Config.Exclude) {
			return nil, otgerror.New(otgerror.CodeSecretPolicy, "secret path is excluded by project policy: "+rel)
		}
		if _, err := os.Lstat(filepath.Join(options.Root, filepath.FromSlash(rel))); err != nil {
			return nil, err
		}
		chosen[rel] = selectedFile{Relative: rel, Secret: true, Policy: "include-secret"}
	}
	result := make([]selectedFile, 0, len(chosen))
	for _, file := range chosen {
		if _, err := packageotg.ProjectPath(file.Relative); err != nil {
			return nil, otgerror.Wrap(otgerror.CodeSecretPolicy, err.Error(), err)
		}
		if err := packageotg.CheckParents(options.Root, file.Relative); err != nil {
			return nil, otgerror.Wrap(otgerror.CodeSecretPolicy, err.Error(), err)
		}
		result = append(result, file)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Relative < result[j].Relative })
	return result, nil
}

func expand(root, raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	rel, err := config.ValidateRelative(root, raw)
	if err != nil {
		return nil, err
	}
	if _, err := packageotg.ProjectPath(rel); err != nil {
		return nil, otgerror.Wrap(otgerror.CodeSecretPolicy, err.Error(), err)
	}
	if err := packageotg.CheckParents(root, rel); err != nil {
		return nil, otgerror.Wrap(otgerror.CodeSecretPolicy, err.Error(), err)
	}
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, nil
	}
	var result []string
	for _, match := range matches {
		err := filepath.WalkDir(match, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			value, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			result = append(result, filepath.ToSlash(value))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func copySelected(root, payload string, selected selectedFile) (snapshot.File, error) {
	if err := packageotg.CheckParents(root, selected.Relative); err != nil {
		return snapshot.File{}, err
	}
	source := filepath.Join(root, filepath.FromSlash(selected.Relative))
	info, err := os.Lstat(source)
	if err != nil {
		return snapshot.File{}, err
	}
	archivePath := "files/" + selected.Relative
	entry := snapshot.File{RelativePath: selected.Relative, ArchivePath: archivePath, Mode: uint32(info.Mode().Perm()), Secret: selected.Secret, PolicySource: selected.Policy}
	destination := filepath.Join(payload, filepath.FromSlash(archivePath))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return snapshot.File{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return snapshot.File{}, err
		}
		if filepath.IsAbs(target) {
			return snapshot.File{}, otgerror.New(otgerror.CodeSecretPolicy, "absolute symlink is not portable: "+selected.Relative)
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(source), target))
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return snapshot.File{}, otgerror.New(otgerror.CodeSecretPolicy, "symlink escapes project root: "+selected.Relative)
		}
		if err := os.Symlink(target, destination); err != nil {
			return snapshot.File{}, err
		}
		entry.Kind = "symlink"
		entry.LinkTarget = target
		entry.SHA256 = hashBytes([]byte(target))
		return entry, nil
	}
	if !info.Mode().IsRegular() {
		return snapshot.File{}, fmt.Errorf("unsupported selected file type: %s", selected.Relative)
	}
	mode := info.Mode().Perm()
	if selected.Secret {
		mode = 0o600
	}
	if err := copyRegular(source, destination, mode); err != nil {
		return snapshot.File{}, err
	}
	entry.Kind = "file"
	entry.SHA256, entry.SizeBytes, err = hashRegular(destination)
	return entry, err
}

func copyContext(source, payload string, manifest *snapshot.Manifest) error {
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() {
		return otgerror.Wrap(otgerror.CodeSessionFormat, "context envelope directory is unavailable", err)
	}
	destination := filepath.Join(payload, "context")
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(destination, rel), 0o700)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("context envelope may contain regular files only")
		}
		target := filepath.Join(destination, rel)
		if err := copyRegular(path, target, 0o600); err != nil {
			return err
		}
		hash, _, err := hashRegular(target)
		if err != nil {
			return err
		}
		manifest.PayloadHashes[filepath.ToSlash(filepath.Join("context", rel))] = hash
		return nil
	})
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(source, "context-envelope.json"))
	if err == nil {
		// Retain optional legacy archive metadata without the retired summary
		// and agent-evidence collection subsystem.
		var envelope struct {
			ID           string `json:"id"`
			AgentKind    string `json:"agent_kind"`
			AgentVersion string `json:"agent_version"`
			SessionID    string `json:"session_id"`
		}
		if json.Unmarshal(data, &envelope) == nil {
			manifest.Agent = snapshot.Agent{Kind: envelope.AgentKind, Version: envelope.AgentVersion, SessionID: envelope.SessionID, ContextEnvelopeID: envelope.ID, ContextSHA256: hashBytes(data)}
		}
	}
	return nil
}

// Fingerprint describes the current workspace without timestamps or generated
// snapshot metadata. It includes selected ignored files and all Git refs, and
// re-discovers untracked files on every call so additions cannot go unnoticed.
func Fingerprint(ctx context.Context, options Options) (string, error) {
	if err := gitx.Validate(ctx, options.Root); err != nil {
		return "", err
	}
	selected, err := selectFiles(ctx, options)
	if err != nil {
		return "", err
	}
	data, err := fingerprint(ctx, options.Root, selected)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func fingerprint(ctx context.Context, root string, selected []selectedFile) ([]byte, error) {
	head, err := gitText(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	index, err := gitx.Run(ctx, root, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return nil, err
	}
	work, err := gitx.Run(ctx, root, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	write := func(data []byte) {
		fmt.Fprintf(hash, "%d:", len(data))
		hash.Write(data)
	}
	write([]byte(head))
	for _, args := range [][]string{{"for-each-ref", "--format=%(refname) %(objectname)"}, {"symbolic-ref", "--quiet", "HEAD"}} {
		result, err := gitx.Run(ctx, root, args...)
		if err != nil && args[0] != "symbolic-ref" {
			return nil, err
		}
		write(result.Stdout)
	}
	write(index.Stdout)
	write(work.Stdout)
	for _, file := range selected {
		write([]byte(file.Relative))
		path := filepath.Join(root, filepath.FromSlash(file.Relative))
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		mode := info.Mode()
		if file.Secret && mode.IsRegular() {
			mode = 0o600
		}
		write([]byte(mode.String()))
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return nil, err
			}
			write([]byte(target))
		} else {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("unsupported selected file type: %s", file.Relative)
			}
			fileHash, _, err := hashRegular(path)
			if err != nil {
				return nil, err
			}
			write([]byte(fileHash))
		}
	}
	return hash.Sum(nil), nil
}

func acquireLock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, otgerror.New(otgerror.CodePrecondition, "another ONTHEGO capture is active")
		}
		return nil, err
	}
	fmt.Fprintf(file, "%d\n", os.Getpid())
	if err := file.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

func gitText(ctx context.Context, root string, args ...string) (string, error) {
	result, err := gitx.Run(ctx, root, args...)
	return strings.TrimSpace(string(result.Stdout)), err
}

func copyRegular(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if err := output.Chmod(mode); err != nil {
		output.Close()
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func hashRegular(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	return hex.EncodeToString(hash.Sum(nil)), size, err
}

func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func excluded(path string, patterns []string) bool {
	if path == ".onthego.local" || strings.HasPrefix(path, ".onthego.local/") || strings.HasSuffix(path, ".otg") {
		return true
	}
	for _, pattern := range patterns {
		clean := filepath.ToSlash(filepath.Clean(pattern))
		if path == clean || strings.HasPrefix(path, strings.TrimSuffix(clean, "/")+"/") {
			return true
		}
		if matched, _ := filepath.Match(pattern, path); matched {
			return true
		}
	}
	return false
}

func alwaysDenied(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := strings.ToLower(filepath.Base(lower))
	return lower == ".git" || strings.HasPrefix(lower, ".git/") || strings.Contains(lower, "/.git/") || strings.HasPrefix(lower, ".ssh/") || strings.Contains(lower, "/.ssh/") || base == "auth.json" || base == "credentials" || base == "credentials.json" || base == ".credentials.json" || base == "id_rsa" || base == "id_ed25519" || strings.HasSuffix(base, ".key") || strings.Contains(base, "private_key")
}

func isSecretCandidate(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") || strings.HasSuffix(base, ".pem") || strings.Contains(base, "secret") || strings.Contains(base, "token")
}

func SigningPublic(keys identity.Keys) string {
	if keys.SigningPublic != "" {
		return keys.SigningPublic
	}
	private, err := base64.StdEncoding.DecodeString(keys.SigningPrivate)
	if err != nil || len(private) < 32 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(private[len(private)-32:])
}
