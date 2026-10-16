package agentsessions

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ImportOptions chooses a policy only for divergent native histories. skip keeps
// the destination; theirs replaces it with incoming bytes; keep-both (default)
// keeps the destination and privately archives incoming bytes outside the repo.
type ImportOptions struct {
	ConflictPolicy string `json:"conflict_policy,omitempty"`
	ConflictsDir   string `json:"conflicts_dir,omitempty"`
}

func conflictPlan(directory string, session Session, incoming []byte) (*writePlan, string, error) {
	if directory == "" {
		home := os.Getenv("ONTHEGO_HOME")
		if home == "" {
			var err error
			home, err = os.UserHomeDir()
			if err != nil {
				return nil, "", err
			}
			home = filepath.Join(home, ".onthego")
		}
		directory = filepath.Join(home, "conflicts")
	}
	home, err := absolute(directory)
	if err != nil {
		return nil, "", err
	}
	// Digest-addressed names avoid untrusted session IDs and make repeats idempotent.
	relative := filepath.ToSlash(filepath.Join(session.Agent, digest(incoming)+".jsonl"))
	if err := noSymlinks(home, relative); err != nil {
		return nil, "", err
	}
	// A preexisting public conflict directory is never used for sensitive content.
	for _, dir := range []string{home, filepath.Join(home, session.Agent)} {
		info, err := os.Lstat(dir)
		if err == nil && (!info.IsDir() || info.Mode().Perm()&0o077 != 0) {
			return nil, "", fmt.Errorf("native session conflict directory must be private (0700): %s", dir)
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, "", err
		}
	}
	path := filepath.Join(home, filepath.FromSlash(relative))
	previous, err := readStable(path, MaxSessionBytes)
	if err == nil {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return nil, "", statErr
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, "", errors.New("existing session conflict archive must be private (0600)")
		}
		if !bytes.Equal(previous, incoming) {
			return nil, "", errors.New("session conflict archive hash collision")
		}
		return nil, path, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", err
	}
	return &writePlan{home: home, relative: relative, incoming: incoming}, path, nil
}
