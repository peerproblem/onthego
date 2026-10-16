package syncer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/otgerror"
)

const pendingTTL = 30 * 24 * time.Hour

func safeTarget(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", otgerror.New(otgerror.CodeInput, "project path is required")
	}
	root, err := fsx.Canonical(raw)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	home, err = fsx.Canonical(home)
	if err != nil {
		return "", err
	}
	dataRoot, err := config.DataRoot()
	if err != nil {
		return "", err
	}
	dataRoot, err = fsx.Canonical(dataRoot)
	if err != nil {
		return "", err
	}
	if root == filepath.Dir(root) || isWithin(root, home) || isWithin(root, dataRoot) || isWithin(dataRoot, root) {
		return "", otgerror.New(otgerror.CodeInput, "project cannot be the filesystem root, home directory, or overlap ONTHEGO's private state")
	}
	return root, nil
}
func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func checkStatePath(path string) error {
	home, err := config.DataRoot()
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return err
	}
	return fsx.CheckPath(home, rel)
}
func lock(root string) (func(), error) {
	home, err := config.DataRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, "locks", digest([]byte(root))+".lock")
	if err := checkStatePath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return fsx.Lock(path)
}
func statePath(root, projectID string) (string, error) {
	base, err := config.ProjectDataRoot(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "handoff", digest([]byte(root)), "current.json"), nil
}
func peerStatePath(root, projectID, peer string) (string, error) {
	current, err := statePath(root, projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(current), "peers", digest([]byte(peer))+".json"), nil
}
func pendingPath(root, projectID, snapshotID string) (string, error) {
	if !validDigest(snapshotID) {
		return "", errors.New("invalid snapshot ID")
	}
	current, err := statePath(root, projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(current), "pending", snapshotID+".json"), nil
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
		return baseline{}, fmt.Errorf("read handoff baseline: %w", err)
	}
	if err := validateBaseline(saved, root, projectID); err != nil {
		return baseline{}, err
	}
	return saved, nil
}
func validateBaseline(saved baseline, root, projectID string) error {
	if saved.SchemaVersion != 2 || saved.Root != root || saved.ProjectID != projectID || !validDigest(saved.SnapshotID) || !validDigest(saved.Fingerprint) {
		return errors.New("invalid handoff baseline")
	}
	for _, path := range append(append(append([]string{}, saved.Managed...), saved.Include...), saved.IncludeSecret...) {
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
	if saved.Peer != "" {
		path, err := peerStatePath(saved.Root, saved.ProjectID, saved.Peer)
		if err != nil {
			return err
		}
		if err := writeJSON(path, saved); err != nil {
			return err
		}
	}
	path, err := statePath(saved.Root, saved.ProjectID)
	if err != nil {
		return err
	}
	return writeJSON(path, saved)
}
func pendingBaselines(root, projectID string) ([]baseline, error) {
	current, err := statePath(root, projectID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(current), "pending")
	if err := checkStatePath(dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []baseline
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var saved baseline
		if err := readJSON(path, &saved); err != nil {
			return nil, err
		}
		if err := validateBaseline(saved, root, projectID); err != nil {
			return nil, err
		}
		if time.Since(saved.UpdatedAt) > pendingTTL {
			if err := os.Remove(path); err != nil {
				return nil, err
			}
			continue
		}
		result = append(result, saved)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	// Pending exports are recoverable for 30 days, bounded to the newest 64.
	for _, saved := range result[min(len(result), 64):] {
		path, _ := pendingPath(root, projectID, saved.SnapshotID)
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	if len(result) > 64 {
		result = result[:64]
	}
	return result, nil
}
func readJSON(path string, value any) error {
	if err := checkStatePath(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}
func writeJSON(path string, value any) error {
	if err := checkStatePath(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".handoff-state-*")
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
