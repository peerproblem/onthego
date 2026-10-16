package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jbaehova/onthego/internal/capture"
	"github.com/jbaehova/onthego/internal/config"
	"github.com/jbaehova/onthego/internal/gitx"
	"github.com/jbaehova/onthego/internal/otgerror"
)

type localFile struct {
	path, hash, link string
	mode             os.FileMode
}

// preserveLocalFiles carries device-local ignored/excluded files into the new checkout.
// A path explicitly managed by either handoff policy is never resurrected here,
// so deleting a synchronized ignored file remains a deletion on the other peer.
func preserveLocalFiles(ctx context.Context, source, target string, previous baseline, incoming config.Config) ([]localFile, error) {
	old, err := config.Load(source)
	if err != nil {
		return nil, err
	}
	preview, err := capture.Preview(ctx, optionsFor(source, old, previous))
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool, len(preview))
	for _, file := range preview {
		selected[file.RelativePath] = true
	}
	// Omitting --exclude-standard includes both Git-ignored and ordinary
	// untracked files; the preview identifies which are actually synchronized.
	ignored, err := gitx.Run(ctx, source, "ls-files", "--others", "-z")
	if err != nil {
		return nil, err
	}
	var preserved []localFile
	for _, item := range bytes.Split(ignored.Stdout, []byte{0}) {
		if len(item) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel, err := config.ValidateRelative(source, string(item))
		if err != nil {
			return nil, err
		}
		if selected[rel] || reservedLocal(rel) || matchesPath(rel, previous.Include) || matchesPath(rel, previous.IncludeSecret) || includedLocal(rel, old) || includedLocal(rel, incoming) {
			continue
		}
		from, to := filepath.Join(source, filepath.FromSlash(rel)), filepath.Join(target, filepath.FromSlash(rel))
		// Incoming files and parent symlinks always take precedence. Never follow
		// a link restored from Git when copying the receiver's local environment.
		available, err := localDestination(target, rel)
		if err != nil {
			return nil, err
		}
		if !available {
			continue
		}
		if err := noSymlinkComponents(filepath.Dir(from)); err != nil {
			return nil, err
		}
		info, err := os.Lstat(from)
		if err != nil {
			return nil, err
		}
		entry := localFile{path: rel, mode: info.Mode()}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.link, err = os.Readlink(from)
			if err != nil {
				return nil, err
			}
			if filepath.IsAbs(entry.link) || !isWithin(source, filepath.Clean(filepath.Join(filepath.Dir(from), entry.link))) {
				return nil, otgerror.New(otgerror.CodePrecondition, "cannot preserve an ignored symlink outside the project: "+rel)
			}
			if err := os.Symlink(entry.link, to); err != nil {
				return nil, err
			}
		} else if info.Mode().IsRegular() {
			entry.hash, err = copyLocal(from, to, info.Mode().Perm())
			if err != nil {
				return nil, err
			}
		} else {
			return nil, otgerror.New(otgerror.CodePrecondition, "cannot preserve an ignored special file: "+rel)
		}
		preserved = append(preserved, entry)
	}
	return preserved, nil
}

func localDestination(root, rel string) (bool, error) {
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	current := root
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil {
			if i == len(parts)-1 || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return false, nil
			}
			continue
		}
		if !os.IsNotExist(err) {
			return false, err
		}
		if i < len(parts)-1 {
			if err := os.Mkdir(current, 0o700); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func includedLocal(path string, cfg config.Config) bool {
	return matchesPath(path, cfg.Include) && !matchesPath(path, cfg.Exclude)
}

func matchesPath(path string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(filepath.Clean(pattern))
		for candidate := path; candidate != "."; candidate = filepath.ToSlash(filepath.Dir(candidate)) {
			if matched, _ := filepath.Match(pattern, candidate); matched {
				return true
			}
		}
	}
	return false
}

func reservedLocal(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == ".git" || part == ".onthego.local" {
			return true
		}
	}
	return false
}

func copyLocal(source, destination string, mode os.FileMode) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	closeErr := output.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	// Restore owner permissions even if the receiver has a stricter umask.
	if err := os.Chmod(destination, mode); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyLocalFiles(root string, files []localFile) error {
	for _, file := range files {
		path := filepath.Join(root, filepath.FromSlash(file.path))
		if err := noSymlinkComponents(filepath.Dir(path)); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode() != file.mode {
			return otgerror.Wrap(otgerror.CodeSourceChanged, "device-local file changed during restore: "+file.path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil || link != file.link {
				return otgerror.Wrap(otgerror.CodeSourceChanged, "device-local symlink changed during restore: "+file.path, err)
			}
			continue
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, input)
		_ = input.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != file.hash {
			return otgerror.New(otgerror.CodeSourceChanged, fmt.Sprintf("device-local file changed during restore: %s", file.path))
		}
	}
	return nil
}
