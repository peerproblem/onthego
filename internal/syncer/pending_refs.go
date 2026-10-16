package syncer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/otgerror"
)

const pendingRefPrefix = "refs/onthego/pending/"

func recordPending(ctx context.Context, root, path string, pending baseline) error {
	if err := validateBaseline(pending, root, pending.ProjectID); err != nil {
		return otgerror.Wrap(otgerror.CodeInternal, "invalid pending handoff", err)
	}
	if !gitx.ValidOID(pending.WIPCommit) {
		return otgerror.New(otgerror.CodeInternal, "invalid pending handoff commit")
	}
	// Publish the receipt before the ref so concurrent orphan collection in a
	// linked worktree can never see a newly created pin without its receipt.
	if err := writeJSON(path, pending); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, root, "update-ref", pendingRefPrefix+pending.SnapshotID, pending.WIPCommit); err != nil {
		return otgerror.Wrap(otgerror.CodeInternal, "preserve pending handoff commit", errors.Join(err, os.Remove(path)))
	}
	return nil
}

func removePending(ctx context.Context, root, path, snapshotID string) error {
	if !validDigest(snapshotID) {
		return otgerror.New(otgerror.CodeInternal, "invalid pending snapshot ID")
	}
	if err := checkStatePath(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := gitx.Run(ctx, root, "update-ref", "-d", pendingRefPrefix+snapshotID); err != nil {
		return otgerror.Wrap(otgerror.CodeInternal, "remove pending handoff pin", err)
	}
	return nil
}

func pendingCommitExists(ctx context.Context, root, oid string) (bool, error) {
	if !gitx.ValidOID(oid) {
		return false, otgerror.New(otgerror.CodeInternal, "invalid pending handoff commit")
	}
	_, err := gitx.Run(ctx, root, "cat-file", "-e", oid+"^{commit}")
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// cat-file -e uses the same exit status for a missing object and several
	// repository errors. Its batch protocol distinguishes absence without
	// depending on localized stderr, while real Git failures still propagate.
	result, probeErr := gitx.RunInput(ctx, root, nil, []byte(oid+"^{commit}\n"), "cat-file", "--batch-check")
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if probeErr == nil && strings.TrimSpace(string(result.Stdout)) == oid+"^{commit} missing" {
		return false, nil
	}
	return false, otgerror.Wrap(otgerror.CodeInternal, "inspect pending handoff commit", errors.Join(err, probeErr))
}

func pruneOrphanPendingRefs(ctx context.Context, root string) error {
	refs, err := gitx.Run(ctx, root, "for-each-ref", "--format=%(refname)", pendingRefPrefix)
	if err != nil {
		return otgerror.Wrap(otgerror.CodeInternal, "list pending handoff pins", err)
	}
	if len(refs.Stdout) == 0 {
		return nil
	}
	dataRoot, err := config.DataRoot()
	if err != nil {
		return err
	}
	projects := filepath.Join(dataRoot, "projects")
	if err := checkStatePath(projects); err != nil {
		return err
	}
	// Refs are shared by linked worktrees, while receipts live below their
	// individual root and project. Keep any pin that has a receipt anywhere in
	// this user's state, including receipts for another worktree or project.
	receipts := map[string]bool{}
	err = filepath.WalkDir(projects, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(projects, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if entry.IsDir() {
			if (len(parts) == 2 && parts[1] != "handoff") || (len(parts) == 4 && parts[3] != "pending") || len(parts) > 4 {
				return filepath.SkipDir
			}
			return nil
		}
		if len(parts) == 5 && parts[1] == "handoff" && parts[3] == "pending" && strings.HasSuffix(parts[4], ".json") {
			id := strings.TrimSuffix(parts[4], ".json")
			if validDigest(id) {
				receipts[id] = true
			}
		}
		return nil
	})
	if err != nil {
		return otgerror.Wrap(otgerror.CodeInternal, "inspect pending handoff receipts", err)
	}
	for _, ref := range strings.Fields(string(refs.Stdout)) {
		id := strings.TrimPrefix(ref, pendingRefPrefix)
		if !validDigest(id) || receipts[id] {
			continue
		}
		if _, err := gitx.Run(ctx, root, "update-ref", "-d", ref); err != nil {
			return otgerror.Wrap(otgerror.CodeInternal, "remove orphan pending handoff pin", err)
		}
	}
	return nil
}
