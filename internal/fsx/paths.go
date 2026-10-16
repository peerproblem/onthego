// Package fsx centralizes filesystem boundaries used by handoff operations.
package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Canonical resolves the existing portion of a path, including operating-system
// aliases such as /tmp and /var on macOS. A missing trailing path is preserved.
func Canonical(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("filesystem path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	var missing []string
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			if len(missing) > 0 {
				if info.Mode()&os.ModeSymlink != 0 {
					info, err = os.Stat(resolved)
					if err != nil {
						return "", err
					}
				}
				if !info.IsDir() {
					return "", fmt.Errorf("path ancestor is not a directory: %s", current)
				}
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", statErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// CheckParents rejects symlinks and non-directories inside root before the
// final component. The final component may be a symlink copied without following
// it. Call CheckPath when the final component must not be a symlink either.
func CheckParents(root, relative string) error {
	return checkPath(root, relative, false)
}

// CheckPath rejects all symlinks beneath root, including the final component.
// Root itself is an explicitly selected boundary and may use a filesystem alias.
func CheckPath(root, relative string) error {
	return checkPath(root, relative, true)
}

func checkPath(root, relative string, leaf bool) error {
	if relative == "" || filepath.IsAbs(relative) || strings.ContainsRune(relative, '\x00') || strings.Contains(relative, "\\") {
		return fmt.Errorf("invalid relative filesystem path: %q", relative)
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid relative filesystem path: %q", relative)
		}
	}
	current, err := Canonical(root)
	if err != nil {
		return err
	}
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		return fmt.Errorf("filesystem root is not a directory: %s", root)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	count := len(parts)
	if !leaf {
		count--
	}
	for i, part := range parts[:count] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path contains a symbolic link inside the selected root: %s", current)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("path ancestor is not a directory: %s", current)
		}
	}
	return nil
}
