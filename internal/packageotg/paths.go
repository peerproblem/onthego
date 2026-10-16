package packageotg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectPath validates a portable worktree path. Snapshot payloads must never
// overwrite Git metadata, including through alternate casing on macOS.
func ProjectPath(raw string) (string, error) {
	rel, err := safeArchivePath(raw)
	if err != nil {
		return "", err
	}
	for _, part := range strings.Split(rel, "/") {
		if strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("Git metadata cannot be included: %s", raw)
		}
	}
	return rel, nil
}

// CheckParents rejects symlink ancestors before a file is read or written.
// A final symlink can be copied as a symlink, but must never be followed.
func CheckParents(root, relative string) error {
	rel, err := safeArchivePath(relative)
	if err != nil {
		return err
	}
	current := root
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path has a non-directory or symlink ancestor: %s", relative)
		}
	}
	return nil
}
