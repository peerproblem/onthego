package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/packageotg"
	"github.com/peerproblem/onthego/internal/snapshot"
)

type Options struct {
	Root             string
	Config           config.Config
	Output           string
	ContextDir       string
	Include          []string
	IncludeSecret    []string
	ParentSnapshotID string
	RecipientTips    []string
	ParentWIPCommit  string
	OnthegoVersion   string
	Recipient        age.Recipient
	Keys             identity.Keys
}

type Result struct {
	Manifest      snapshot.Manifest `json:"manifest"`
	Output        string            `json:"output"`
	ExcludedFiles []string          `json:"excluded_files,omitempty"`
	Fingerprint   string            `json:"fingerprint"`
}

type PreviewFile struct {
	RelativePath string `json:"relative_path"`
	Secret       bool   `json:"secret"`
	Policy       string `json:"policy"`
	Excluded     bool   `json:"excluded,omitempty"`
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
	excluded, err := excludedFiles(ctx, options, selected)
	if err != nil {
		return nil, err
	}
	for _, path := range excluded {
		result = append(result, PreviewFile{RelativePath: path, Secret: true, Policy: "secret candidate; use --include-secret for permitted files", Excluded: true})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RelativePath < result[j].RelativePath })
	return result, nil
}

type selectedFile struct {
	Relative string
	Secret   bool
	Policy   string
}

func Create(ctx context.Context, options Options) (Result, error) {
	root, err := fsx.Canonical(options.Root)
	if err != nil {
		return Result{}, err
	}
	options.Root = root
	if err := gitx.Validate(ctx, root); err != nil {
		return Result{}, err
	}
	if options.Output == "" {
		return Result{}, otgerror.New(otgerror.CodeInput, "snapshot output path is required")
	}
	if options.Recipient == nil {
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
	if err = os.MkdirAll(dataRoot, 0700); err != nil {
		return Result{}, err
	}
	unlock, err := fsx.Lock(filepath.Join(dataRoot, "capture.lock"))
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
	omitted, err := excludedFiles(ctx, options, selected)
	if err != nil {
		return Result{}, err
	}
	home, err := config.DataRoot()
	if err != nil {
		return Result{}, err
	}
	temp, err := fsx.TempDir(home, "capture-")
	if err != nil {
		return Result{}, err
	}
	defer fsx.ReleaseTemp(temp)
	payload := filepath.Join(temp, "payload")
	if err = os.Mkdir(payload, 0700); err != nil {
		return Result{}, err
	}
	manifest := snapshot.Manifest{SchemaVersion: snapshot.SchemaVersion, ProjectID: options.Config.ProjectID, CreatedAt: time.Now().UTC(), PayloadHashes: map[string]string{}, SignerPublic: options.Keys.SigningPublic, Toolchain: snapshot.Toolchain{Onthego: options.OnthegoVersion}}
	if options.ParentSnapshotID != "" {
		manifest.ParentSnapshotIDs = []string{options.ParentSnapshotID}
	}
	manifest.Toolchain.GitVersion, _ = gitx.Version(ctx)
	var extra []string
	for _, file := range selected {
		entry, e := describeSelected(root, file)
		if e != nil {
			return Result{}, e
		}
		if entry.Secret {
			entry.ArchivePath = "secret-files/" + entry.RelativePath
			destination := filepath.Join(payload, filepath.FromSlash(entry.ArchivePath))
			if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
				return Result{}, err
			}
			if entry.Kind == "symlink" {
				err = os.WriteFile(destination, []byte(entry.LinkTarget), 0600)
			} else {
				err = fsx.CopyRegular(filepath.Join(root, filepath.FromSlash(entry.RelativePath)), destination, 0600)
			}
			if err != nil {
				return Result{}, err
			}
			manifest.PayloadHashes[entry.ArchivePath] = entry.SHA256
		} else {
			extra = append(extra, file.Relative)
		}
		manifest.Files = append(manifest.Files, entry)
	}
	state, err := gitx.CaptureWIP(ctx, root, temp, extra, options.ParentWIPCommit)
	if err != nil {
		return Result{}, err
	}
	manifest.SourceGitHead = state.Head
	manifest.SourceBranch = state.Branch
	manifest.Git = snapshot.Git{Ref: state.Branch, WIPCommit: state.Commit, IndexTree: state.IndexTree, WorktreeTree: state.WorktreeTree, UntrackedTree: state.UntrackedTree}
	if origin, e := gitText(ctx, root, "remote", "get-url", "origin"); e == nil {
		manifest.Git.OriginURL = gitx.SafeOriginURL(origin)
	}
	remaining := packageotg.MaxPayloadSize
	for _, file := range manifest.Files {
		if file.Secret {
			remaining -= file.SizeBytes
		}
	}
	if remaining < 0 {
		return Result{}, fmt.Errorf("secret files exceed workspace payload limit")
	}
	if err = gitx.CheckWorkspaceSize(ctx, root, state.Head, []string{state.IndexTree, state.WorktreeTree}, state.UntrackedTree, remaining); err != nil {
		return Result{}, err
	}
	bundle := filepath.Join(payload, "repo.bundle")
	if err = gitx.Bundle(ctx, root, bundle, state.Commit, options.RecipientTips); err != nil {
		return Result{}, err
	}
	hash, _, err := hashRegular(bundle)
	if err != nil {
		return Result{}, err
	}
	manifest.Git.BundleSHA256 = hash
	manifest.PayloadHashes["repo.bundle"] = hash
	if options.ContextDir != "" {
		if err = copyContext(options.ContextDir, payload, &manifest); err != nil {
			return Result{}, err
		}
	}
	if err = manifest.SetSnapshotID(); err != nil {
		return Result{}, err
	}
	after, err := Fingerprint(ctx, options)
	if err != nil {
		return Result{}, err
	}
	if before != after {
		return Result{}, &otgerror.Error{Code: otgerror.CodeSourceChanged, Message: "project changed while the snapshot was being captured", Retryable: true, Stage: "capture"}
	}
	if err = os.MkdirAll(filepath.Dir(options.Output), 0700); err != nil {
		return Result{}, err
	}
	if _, err = os.Lstat(options.Output); err == nil {
		return Result{}, otgerror.New(otgerror.CodeInput, "snapshot output already exists")
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	if err = packageotg.CreateContext(ctx, payload, options.Output, manifest, options.Recipient, signer); err != nil {
		return Result{}, err
	}
	return Result{Manifest: manifest, Output: options.Output, ExcludedFiles: omitted, Fingerprint: before}, nil
}

func excludedFiles(ctx context.Context, options Options, selected []selectedFile) ([]string, error) {
	accepted := map[string]bool{}
	for _, f := range selected {
		accepted[f.Relative] = true
	}
	r, e := gitx.Run(ctx, options.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if e != nil {
		return nil, e
	}
	var out []string
	for _, p := range bytes.Split(r.Stdout, []byte{0}) {
		s := string(p)
		if s != "" && !accepted[s] && !excluded(s, options.Config.Exclude) && (isSecretCandidate(s) || alwaysDenied(s)) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out, nil
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

func describeSelected(root string, selected selectedFile) (snapshot.File, error) {
	if err := packageotg.CheckParents(root, selected.Relative); err != nil {
		return snapshot.File{}, err
	}
	source := filepath.Join(root, filepath.FromSlash(selected.Relative))
	info, err := os.Lstat(source)
	if err != nil {
		return snapshot.File{}, err
	}
	entry := snapshot.File{RelativePath: selected.Relative, Mode: uint32(info.Mode().Perm()), Secret: selected.Secret, PolicySource: selected.Policy, SizeBytes: info.Size()}
	if info.Mode()&os.ModeSymlink != 0 {
		target, e := os.Readlink(source)
		if e != nil {
			return entry, e
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(source), target))
		rel, e := filepath.Rel(root, resolved)
		if filepath.IsAbs(target) || e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return entry, otgerror.New(otgerror.CodeSecretPolicy, "symlink escapes project root: "+selected.Relative)
		}
		entry.Kind = "symlink"
		entry.LinkTarget = target
		entry.SHA256 = hashBytes([]byte(target))
		return entry, nil
	}
	if !info.Mode().IsRegular() {
		return entry, fmt.Errorf("unsupported selected file type: %s", selected.Relative)
	}
	entry.Kind = "file"
	entry.SHA256, entry.SizeBytes, err = hashRegular(source)
	if selected.Secret {
		entry.Mode = 0600
	}
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
	return nil
}

// Fingerprint describes the current workspace without timestamps or generated
// snapshot metadata. It includes selected ignored files and the active branch state, and
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
	index, err := gitx.Run(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	write := func(data []byte) { fmt.Fprintf(hash, "%d:", len(data)); hash.Write(data) }
	write([]byte(head))
	write(index.Stdout)
	branch, err := gitx.Run(ctx, root, "symbolic-ref", "--quiet", "HEAD")
	if err != nil && len(branch.Stderr) > 0 {
		return nil, err
	}
	write(branch.Stdout)
	secret := map[string]bool{}
	for _, file := range selected {
		if file.Secret {
			secret[file.Relative] = true
		}
	}
	seen := map[string]bool{}
	leaf := func(path string, missingOK bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		write([]byte(path))
		absolute := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(absolute)
		if missingOK && (os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)) {
			write([]byte("missing"))
			return nil
		}
		if err != nil {
			return err
		}
		if err = fsx.CheckParents(root, path); err != nil {
			return err
		}
		if missingOK && info.IsDir() {
			write([]byte("missing"))
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(absolute)
			if err != nil {
				return err
			}
			write([]byte("120000"))
			write([]byte(target))
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported workspace file type: %s", path)
		}
		mode := "100644"
		if info.Mode().Perm()&0111 != 0 && !secret[path] {
			mode = "100755"
		}
		write([]byte(mode))
		digest, _, err := fsx.HashRegular(absolute)
		if err != nil {
			return err
		}
		write([]byte(digest))
		return nil
	}
	for _, entry := range bytes.Split(index.Stdout, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		parts := bytes.SplitN(entry, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid Git index entry")
		}
		path := string(parts[1])
		seen[path] = true
		if err = leaf(path, true); err != nil {
			return nil, err
		}
	}
	for _, file := range selected {
		if seen[file.Relative] {
			continue
		}
		seen[file.Relative] = true
		if err = leaf(file.Relative, false); err != nil {
			return nil, err
		}
	}
	return hash.Sum(nil), nil
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
	return lower == ".git" || strings.HasPrefix(lower, ".git/") || strings.Contains(lower, "/.git/") || strings.HasPrefix(lower, ".ssh/") || strings.Contains(lower, "/.ssh/") || base == "auth.json" || base == "credentials" || base == "credentials.json" || base == ".credentials.json" || base == "id_rsa" || base == "id_ed25519"
}

func isSecretCandidate(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") || strings.HasSuffix(base, ".pem") || strings.HasPrefix(base, "id_") || strings.HasSuffix(base, ".p12")
}
