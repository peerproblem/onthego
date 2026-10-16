package packageotg

import (
	"fmt"
	"github.com/peerproblem/onthego/internal/fsx"
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
func CheckParents(root, relative string) error { return fsx.CheckParents(root, relative) }
