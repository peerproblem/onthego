package syncer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jbaehova/onthego/internal/config"
	"github.com/jbaehova/onthego/internal/otgerror"
)

func safeTarget(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", otgerror.New(otgerror.CodeInput, "project path is required")
	}
	root, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return "", err
	}
	dataRoot, err := config.DataRoot()
	if err != nil {
		return "", err
	}
	if root == filepath.Dir(root) || isWithin(root, home) || isWithin(root, dataRoot) {
		return "", otgerror.New(otgerror.CodeInput, "project cannot be the filesystem root, home directory, or contain ONTHEGO's private state")
	}
	if err := noSymlinkComponents(root); err != nil {
		return "", err
	}
	return root, nil
}

func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func noSymlinkComponents(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return otgerror.New(otgerror.CodeInput, "symbolic link in project or state path is not allowed: "+current+"; use its physical path (pwd -P)")
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func lock(root string) (func(), error) {
	dataRoot, err := config.DataRoot()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(dataRoot, "locks")
	if err := noSymlinkComponents(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, digest([]byte(root))+".lock")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return nil, otgerror.New(otgerror.CodePrecondition, "another handoff is active for this project path; if it crashed, inspect and remove its stale lock: "+path)
	}
	if err != nil {
		return nil, err
	}
	_, writeErr := fmt.Fprintf(file, "%d\n", os.Getpid())
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

func statePath(root, projectID string) (string, error) {
	base, err := config.ProjectDataRoot(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sync", digest([]byte(root))+".json"), nil
}

func pendingPath(root, projectID, snapshotID string) (string, error) {
	if !validDigest(snapshotID) {
		return "", errors.New("invalid snapshot ID")
	}
	base, err := config.ProjectDataRoot(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sync", "pending", digest([]byte(root)), snapshotID+".json"), nil
}

func readBaseline(root, projectID string) (baseline, error) {
	path, err := statePath(root, projectID)
	if err != nil {
		return baseline{}, err
	}
	var saved baseline
	if err := readJSON(path, &saved); err != nil {
		if os.IsNotExist(err) {
			return baseline{}, nil
		}
		return baseline{}, fmt.Errorf("read synchronization baseline: %w", err)
	}
	if err := validateBaseline(saved, root, projectID); err != nil {
		return baseline{}, err
	}
	return saved, nil
}

func validateBaseline(saved baseline, root, projectID string) error {
	if saved.SchemaVersion != 1 || saved.Root != root || saved.ProjectID != projectID || !validDigest(saved.SnapshotID) || !validDigest(saved.Fingerprint) {
		return errors.New("invalid synchronization baseline")
	}
	for _, ancestor := range saved.Ancestors {
		if !validDigest(ancestor) {
			return errors.New("invalid synchronization ancestor")
		}
	}
	for _, path := range append(append([]string{}, saved.Include...), saved.IncludeSecret...) {
		if _, err := config.ValidateRelative(root, path); err != nil {
			return err
		}
	}
	return nil
}

func validDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil
}

func saveBaseline(saved baseline) error {
	if err := validateBaseline(saved, saved.Root, saved.ProjectID); err != nil {
		return err
	}
	path, err := statePath(saved.Root, saved.ProjectID)
	if err != nil {
		return err
	}
	return writeJSON(path, saved)
}

func readJSON(path string, value any) error {
	if err := noSymlinkComponents(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSON(path string, value any) error {
	if err := noSymlinkComponents(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".sync-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(append(data, '\n'))
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
