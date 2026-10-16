package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/jbaehova/onthego/internal/gitx"
	"github.com/jbaehova/onthego/internal/otgerror"
	"github.com/jbaehova/onthego/internal/packageotg"
	"github.com/jbaehova/onthego/internal/snapshot"
)

type Result struct {
	Manifest snapshot.Manifest `json:"manifest"`
	Root     string            `json:"root"`
}

func Package(ctx context.Context, packagePath, output string, ageIdentity age.Identity, expectedSigner string) (Result, error) {
	if _, err := os.Lstat(output); err == nil {
		return Result{}, otgerror.New(otgerror.CodeInput, "restore output already exists")
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Result{}, err
	}
	staging, err := os.MkdirTemp(parent, ".onthego-restore-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(staging)
	extracted := filepath.Join(staging, "payload")
	manifest, err := packageotg.Extract(packagePath, extracted, ageIdentity, expectedSigner)
	if err != nil {
		return Result{}, err
	}
	root := filepath.Join(staging, "repo")
	command := []string{"-c", "core.autocrlf=false", "clone", "--no-checkout", "--no-hardlinks", filepath.Join(extracted, "repo.bundle"), root}
	if _, err := gitx.Run(ctx, parent, command...); err != nil {
		return Result{}, restoreError("clone bundled repository", err)
	}
	if _, err := gitx.Run(ctx, root, "remote", "remove", "origin"); err != nil {
		return Result{}, restoreError("remove temporary bundle remote", err)
	}
	// Clone maps local branches to remote-tracking refs. Fetch the complete ref
	// namespace back into place, including unpushed branches, tags and stashes.
	if _, err := gitx.Run(ctx, root, "fetch", "--no-tags", "--update-head-ok", filepath.Join(extracted, "repo.bundle"), "+refs/*:refs/*"); err != nil {
		return Result{}, restoreError("restore bundled Git refs", err)
	}
	checkout := []string{"-c", "core.autocrlf=false", "checkout", "--detach", manifest.SourceGitHead}
	if manifest.SourceBranch != "" && manifest.SourceBranch != "(detached)" {
		if _, err := gitx.Run(ctx, root, "check-ref-format", "--branch", manifest.SourceBranch); err != nil {
			return Result{}, restoreError("snapshot branch is invalid", err)
		}
		checkout = []string{"-c", "core.autocrlf=false", "checkout", "-B", manifest.SourceBranch, manifest.SourceGitHead}
	}
	if _, err := gitx.Run(ctx, root, checkout...); err != nil {
		return Result{}, restoreError("checkout snapshot HEAD", err)
	}
	if manifest.Git.OriginURL != "" {
		if gitx.SafeOriginURL(manifest.Git.OriginURL) != manifest.Git.OriginURL {
			return Result{}, restoreError("snapshot origin URL is not portable or contains credentials", nil)
		}
		if _, err := gitx.Run(ctx, root, "remote", "add", "origin", manifest.Git.OriginURL); err != nil {
			return Result{}, restoreError("restore origin remote", err)
		}
	}
	if err := applyPatch(ctx, root, filepath.Join(extracted, "index.patch"), true); err != nil {
		return Result{}, err
	}
	if err := applyPatch(ctx, root, filepath.Join(extracted, "worktree.patch"), false); err != nil {
		return Result{}, err
	}
	for _, file := range manifest.Files {
		if err := restoreSelected(extracted, root, file); err != nil {
			return Result{}, err
		}
	}
	contextSource := filepath.Join(extracted, "context")
	if info, err := os.Stat(contextSource); err == nil && info.IsDir() {
		if err := packageotg.CheckParents(root, ".onthego.local/context/placeholder"); err != nil {
			return Result{}, restoreError("context destination traverses a symlink", err)
		}
		if err := copyTree(contextSource, filepath.Join(root, ".onthego.local", "context")); err != nil {
			return Result{}, err
		}
	}
	if err := verify(ctx, root, extracted, manifest); err != nil {
		return Result{}, err
	}
	if err := os.Rename(root, output); err != nil {
		return Result{}, err
	}
	return Result{Manifest: manifest, Root: output}, nil
}

func applyPatch(ctx context.Context, root, path string, index bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return restoreError("read patch", err)
	}
	if info.Size() == 0 {
		return nil
	}
	args := []string{"-c", "core.autocrlf=false", "apply", "--binary", "--whitespace=nowarn"}
	if index {
		args = append(args, "--index")
	}
	args = append(args, path)
	if _, err := gitx.Run(ctx, root, args...); err != nil {
		return restoreError("apply captured Git patch", err)
	}
	return nil
}

func restoreSelected(extracted, root string, file snapshot.File) error {
	rel, err := packageotg.ProjectPath(file.RelativePath)
	if err != nil {
		return restoreError("manifest contains unsafe selected path", err)
	}
	if err := packageotg.CheckParents(root, rel); err != nil {
		return restoreError("selected path traverses a symlink", err)
	}
	destination := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	if file.Kind == "tombstone" {
		return nil
	}
	source := filepath.Join(extracted, filepath.FromSlash(file.ArchivePath))
	if file.Kind == "symlink" {
		if filepath.IsAbs(file.LinkTarget) {
			return restoreError("absolute selected symlink", nil)
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(destination), file.LinkTarget))
		outside, err := filepath.Rel(root, resolved)
		if err != nil || outside == ".." || strings.HasPrefix(outside, ".."+string(filepath.Separator)) {
			return restoreError("selected symlink escapes restore root", err)
		}
		return os.Symlink(file.LinkTarget, destination)
	}
	mode := fs.FileMode(file.Mode) & 0o777
	if file.Secret {
		mode = 0o600
	}
	if mode == 0 {
		mode = 0o600
	}
	return copyRegular(source, destination, mode)
}

func verify(ctx context.Context, root, extracted string, manifest snapshot.Manifest) error {
	head, err := gitx.Run(ctx, root, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head.Stdout)) != manifest.SourceGitHead {
		return restoreError("restored HEAD does not match manifest", err)
	}
	checks := []struct {
		args     []string
		expected string
	}{
		{[]string{"diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv"}, manifest.Git.IndexPatchSHA256},
		{[]string{"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv"}, manifest.Git.WorkPatchSHA256},
	}
	for _, check := range checks {
		result, err := gitx.Run(ctx, root, check.args...)
		if err != nil || hashBytes(result.Stdout) != check.expected {
			return restoreError("restored Git diff does not match manifest", err)
		}
	}
	for _, file := range manifest.Files {
		path := filepath.Join(root, filepath.FromSlash(file.RelativePath))
		if file.Kind == "symlink" {
			target, err := os.Readlink(path)
			if err != nil || target != file.LinkTarget {
				return restoreError("restored symlink does not match manifest", err)
			}
			continue
		}
		if file.Kind == "tombstone" {
			continue
		}
		hash, err := hashFile(path)
		if err != nil || hash != file.SHA256 {
			return restoreError("restored selected file does not match manifest", err)
		}
	}
	_ = extracted
	return nil
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return restoreError("context contains unsupported entry", nil)
		}
		return copyRegular(path, target, 0o600)
	})
}

func copyRegular(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
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

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func restoreError(message string, err error) error {
	return &otgerror.Error{Code: otgerror.CodeRestoreFailed, Message: message, Cause: err, Stage: "restore"}
}
