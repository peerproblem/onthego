package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peerproblem/onthego/internal/otgerror"
)

type tempLease struct {
	base, marker string
	unlock       func()
}

var tempLeases sync.Map

// TempDir creates a private temporary directory beneath home/tmp and holds a
// kernel lease until ReleaseTemp or process exit. Lease files are kept outside
// payload directories so they cannot enter archives. Defer ReleaseTemp(path).
func TempDir(home, prefix string) (string, error) {
	if strings.ContainsAny(prefix, "/\\") {
		return "", errors.New("temporary directory prefix must not contain path separators")
	}
	base, err := tempBase(home, true)
	if err != nil {
		return "", err
	}
	guard, err := lockFile(filepath.Join(base, ".cleanup.lock"), false)
	if err != nil {
		return "", err
	}
	defer guard()
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(base, ".leases", filepath.Base(dir)+".lock")
	unlock, err := Lock(marker)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	tempLeases.Store(dir, tempLease{base: base, marker: marker, unlock: unlock})
	return dir, nil
}

// ReleaseTemp removes a directory allocated by TempDir and releases its lease.
// It is safe to call more than once and after the directory was already removed.
func ReleaseTemp(path string) error {
	value, ok := tempLeases.LoadAndDelete(path)
	if !ok {
		return nil
	}
	lease := value.(tempLease)
	guard, err := lockFile(filepath.Join(lease.base, ".cleanup.lock"), false)
	if err != nil {
		lease.unlock()
		return err
	}
	defer guard()
	err = os.RemoveAll(path)
	lease.unlock()
	if err == nil {
		err = os.Remove(lease.marker)
		if os.IsNotExist(err) {
			err = nil
		}
	}
	return err
}

// CleanupTemps removes expired directories allocated by TempDir, never an
// active process's directory. Unknown directories and symlinks are left alone.
// A killed process's lease is released by the kernel and can be pruned next run.
func CleanupTemps(home string, maxAge time.Duration) error {
	if maxAge <= 0 {
		return errors.New("temporary directory maximum age must be positive")
	}
	base, err := tempBase(home, false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	guard, err := Lock(filepath.Join(base, ".cleanup.lock"))
	if err != nil {
		if isLocked(err) {
			return nil
		}
		return err
	}
	defer guard()
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-maxAge)
	var failures []error
	for _, entry := range entries {
		if entry.Name() == ".leases" || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		info, err := entry.Info()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		marker := filepath.Join(base, ".leases", entry.Name()+".lock")
		if info, err := os.Lstat(marker); err != nil || !info.Mode().IsRegular() {
			continue
		}
		unlock, err := Lock(marker)
		if err != nil {
			if !isLocked(err) {
				failures = append(failures, err)
			}
			continue
		}
		err = os.RemoveAll(dir)
		unlock()
		if err == nil {
			err = os.Remove(marker)
		}
		if err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func isLocked(err error) bool {
	var typed *otgerror.Error
	return errors.As(err, &typed) && typed.Code == otgerror.CodePrecondition
}

func tempBase(home string, create bool) (string, error) {
	root, err := Canonical(home)
	if err != nil {
		return "", err
	}
	if err := CheckPath(root, "tmp/.leases"); err != nil {
		return "", err
	}
	for _, dir := range []string{root, filepath.Join(root, "tmp"), filepath.Join(root, "tmp", ".leases")} {
		if create {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("private temporary path is not a directory: %s", dir)
		}
		if create {
			if err := os.Chmod(dir, 0o700); err != nil {
				return "", err
			}
		}
	}
	return filepath.Join(root, "tmp"), nil
}
