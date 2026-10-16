package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peerproblem/onthego/internal/gitx"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// pathKeyer probes the destination filesystem instead of inferring its behavior
// from the operating system. The private directory is on the destination volume.
func pathKeyer(root string) (func(string) string, error) {
	probe, err := os.MkdirTemp(root, ".onthego-path-probe-*")
	if err != nil {
		return nil, restoreError("probe destination filesystem", err)
	}
	defer os.RemoveAll(probe)
	same := func(name, alias string) (bool, error) {
		path := filepath.Join(probe, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return false, err
		}
		if err = file.Close(); err != nil {
			return false, err
		}
		original, err := os.Lstat(path)
		if err != nil {
			return false, err
		}
		other, err := os.Lstat(filepath.Join(probe, alias))
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return os.SameFile(original, other), nil
	}
	fold, err := same("case-probe", "CASE-PROBE")
	if err != nil {
		return nil, restoreError("probe destination case handling", err)
	}
	normalize, err := same("normalization-\u00e9", "normalization-e\u0301")
	if err != nil {
		return nil, restoreError("probe destination Unicode normalization", err)
	}
	return filesystemPathKey(fold, normalize), nil
}

func filesystemPathKey(fold, normalize bool) func(string) string {
	folder := cases.Fold()
	return func(path string) string {
		if fold {
			path = folder.String(path)
		}
		if normalize {
			path = norm.NFD.String(path)
		}
		return path
	}
}

// existingPathSpelling resolves each directory entry without changing the
// filesystem. Callers must validate the path and reject symlink parents first.
func existingPathSpelling(root, path string, key func(string) string) (string, error) {
	parts := strings.Split(path, "/")
	directory := root
	for i, part := range parts {
		original, err := os.Lstat(filepath.Join(directory, part))
		if err != nil {
			return "", err
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			return "", err
		}
		found := false
		for _, entry := range entries {
			if key(entry.Name()) == key(part) {
				info, err := entry.Info()
				if err != nil {
					return "", err
				}
				if !os.SameFile(original, info) {
					continue
				}
				parts[i] = entry.Name()
				found = true
				break
			}
		}
		if !found {
			return "", &os.PathError{Op: "resolve backup path", Path: filepath.Join(directory, part), Err: os.ErrNotExist}
		}
		directory = filepath.Join(directory, parts[i])
	}
	return strings.Join(parts, "/"), nil
}

func checkFilesystemPaths(desired map[string]gitx.TreeEntry, key func(string) string) error {
	paths := make([]string, 0, len(desired))
	for path := range desired {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	seen := map[string]string{}
	var collisions []string
	for _, path := range paths {
		k := key(path)
		if prior, ok := seen[k]; ok && prior != path && len(collisions) < 10 {
			collisions = append(collisions, fmt.Sprintf("%q and %q", prior, path))
		} else {
			seen[k] = path
		}
	}
	for _, path := range paths {
		for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			if prior, ok := seen[key(parent)]; ok && len(collisions) < 10 {
				collisions = append(collisions, fmt.Sprintf("%q and %q", prior, path))
			}
		}
	}
	if len(collisions) != 0 {
		return restoreError("paths collide on this filesystem: "+strings.Join(collisions, "; ")+"; rename the conflicting paths in Git before handing off", nil)
	}
	return nil
}

// Check before replacing a destination as well as after writing it. Atomic
// rename gives each replacement a fresh inode, so comparing only post-write
// FileInfo values would overlook an alias that displaced an earlier write.
type writtenPath struct {
	path string
	info os.FileInfo
}

func checkWrittenAlias(path string, info os.FileInfo, written []writtenPath) error {
	for _, previous := range written {
		if previous.path != path && os.SameFile(previous.info, info) {
			return restoreError(fmt.Sprintf("paths collide on this filesystem: %q and %q; restore the backup with onthego undo", previous.path, path), nil)
		}
	}
	return nil
}

// replaceFile stages the complete file or symlink beside its destination. A
// failure before rename leaves the old leaf and its bytes untouched.
func replaceFile(path string, content []byte, mode os.FileMode, symlink bool) error {
	return replaceFileStaged(path, mode, symlink, string(content), func(file *os.File) error {
		_, err := file.Write(content)
		return err
	})
}

func replaceFileStaged(path string, mode os.FileMode, symlink bool, target string, write func(*os.File) error) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".onthego-apply-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if symlink {
		if err = temporary.Close(); err != nil {
			return err
		}
		if err = os.Remove(name); err != nil {
			return err
		}
		if err = os.Symlink(target, name); err != nil {
			return err
		}
	} else {
		if err = temporary.Chmod(mode); err != nil {
			temporary.Close()
			return err
		}
		if err = write(temporary); err != nil {
			temporary.Close()
			return err
		}
		if err = temporary.Sync(); err != nil {
			temporary.Close()
			return err
		}
		if err = temporary.Close(); err != nil {
			return err
		}
	}
	// Directory-to-file transitions have already been proven managed and had
	// their leaves removed. Rename cannot replace a directory with a file.
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && info.IsDir() {
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return nil
}

func incomingPolicyError(message string, cause error) error {
	return packageInvalid(message, cause)
}
