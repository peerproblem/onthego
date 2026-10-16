package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/snapshot"
)

// Recovery state is separate so existing schema-2 workspace receipts remain readable.
type sessionRecovery struct {
	SnapshotID  string    `json:"snapshot_id"`
	RecoveryDir string    `json:"recovery_dir"`
	CreatedAt   time.Time `json:"created_at"`
	Error       string    `json:"error"`
}

func recoveryPath(root, projectID string) (string, error) {
	current, err := statePath(root, projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(current), "session-recovery.json"), nil
}

type sessionRecoveryState struct {
	Pending []sessionRecovery `json:"pending"`
}

func readRecoveries(root, projectID string) ([]sessionRecovery, error) {
	path, err := recoveryPath(root, projectID)
	if err != nil {
		return nil, err
	}
	var state sessionRecoveryState
	if err := readJSON(path, &state); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	home, err := config.DataRoot()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, recovery := range state.Pending {
		if !validDigest(recovery.SnapshotID) || seen[recovery.SnapshotID] || filepath.Dir(recovery.RecoveryDir) != home || !strings.HasPrefix(filepath.Base(recovery.RecoveryDir), "session-recovery-"+recovery.SnapshotID[:12]+"-") {
			return nil, otgerror.New(otgerror.CodePrecondition, "invalid session recovery record")
		}
		if err := checkStatePath(recovery.RecoveryDir); err != nil {
			return nil, err
		}
		seen[recovery.SnapshotID] = true
	}
	return state.Pending, nil
}

func readRecovery(root, projectID string) (sessionRecovery, error) {
	pending, err := readRecoveries(root, projectID)
	if err != nil || len(pending) == 0 {
		return sessionRecovery{}, err
	}
	return pending[len(pending)-1], nil
}

func saveRecovery(root, projectID string, recovery sessionRecovery) error {
	path, err := recoveryPath(root, projectID)
	if err != nil {
		return err
	}
	pending, err := readRecoveries(root, projectID)
	if err != nil {
		return err
	}
	for i, previous := range pending {
		if previous.SnapshotID == recovery.SnapshotID {
			pending[i] = recovery
			return writeJSON(path, sessionRecoveryState{Pending: pending})
		}
	}
	return writeJSON(path, sessionRecoveryState{Pending: append(pending, recovery)})
}

func clearRecovery(root, projectID string, recovery sessionRecovery) error {
	if err := checkStatePath(recovery.RecoveryDir); err != nil {
		return err
	}
	pending, err := readRecoveries(root, projectID)
	if err != nil {
		return err
	}
	var remaining []sessionRecovery
	for _, entry := range pending {
		if entry.SnapshotID != recovery.SnapshotID {
			remaining = append(remaining, entry)
		}
	}
	if err := os.RemoveAll(recovery.RecoveryDir); err != nil {
		return err
	}
	path, err := recoveryPath(root, projectID)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return writeJSON(path, sessionRecoveryState{Pending: remaining})
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RecoverSessions retries authenticated private session data without applying
// workspace files, Git state or the saved workspace fingerprint.
func RecoverSessions(ctx context.Context, root string, options agentsessions.ImportOptions) (Result, error) {
	root, err := safeTarget(root)
	if err != nil {
		return Result{}, err
	}
	if err := gitx.Validate(ctx, root); err != nil {
		return Result{}, err
	}
	unlock, err := lock(root)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	cfg, err := config.Load(root)
	if err != nil {
		return Result{}, err
	}
	previous, err := readBaseline(root, cfg.ProjectID)
	if err != nil {
		return Result{}, err
	}
	pending, err := readRecoveries(root, cfg.ProjectID)
	if err != nil {
		return Result{}, err
	}
	if len(pending) == 0 {
		return Result{}, otgerror.New(otgerror.CodeInput, "no pending session recovery for this workspace")
	}
	result := Result{Root: root, AlreadyApplied: true, SessionsRetried: true}
	for _, recovery := range pending {
		manifest := snapshot.Manifest{ProjectID: cfg.ProjectID, SnapshotID: recovery.SnapshotID}
		retried, err := retrySessions(ctx, root, cfg, previous, recovery, recovery.RecoveryDir, manifest, options)
		if err != nil {
			return result, err
		}
		result.Manifest = manifest
		result.Sessions.Imported += retried.Sessions.Imported
		result.Sessions.Updated += retried.Sessions.Updated
		result.Sessions.Skipped += retried.Sessions.Skipped
		result.Sessions.Conflicts += retried.Sessions.Conflicts
		result.Sessions.Sessions = append(result.Sessions.Sessions, retried.Sessions.Sessions...)
		result.Sessions.Warnings = append(result.Sessions.Warnings, retried.Sessions.Warnings...)
		result.Sessions.PreservedPaths = append(result.Sessions.PreservedPaths, retried.Sessions.PreservedPaths...)
		result.Warnings = append(result.Warnings, retried.Warnings...)
		if retried.Sessions.Status == "recovery_needed" {
			result.Sessions.Status = "recovery_needed"
		}
	}
	if result.Sessions.Status == "" {
		result.Sessions.Status = "recovered"
	}
	return result, nil
}

func retrySessions(ctx context.Context, root string, cfg config.Config, previous baseline, recovery sessionRecovery, native string, manifest snapshot.Manifest, options agentsessions.ImportOptions) (Result, error) {
	result := Result{Manifest: manifest, Root: root, AlreadyApplied: true, SessionsRetried: true}
	switch options.ConflictPolicy {
	case "", "skip", "theirs", "keep-both":
	default:
		return result, otgerror.New(otgerror.CodeInput, "session policy must be skip, theirs, or keep-both")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	roots, err := SessionRoots(cfg)
	if err != nil {
		return result, err
	}
	home, err := config.DataRoot()
	if err != nil {
		return result, err
	}
	if options.ConflictsDir == "" {
		options.ConflictsDir = filepath.Join(home, "conflicts")
	}
	result.Sessions, err = agentsessions.ImportWithOptions(native, root, roots, options)
	if err != nil {
		result.Sessions.Status = "recovery_needed"
		recovery.Error = otgerror.Public(err).Message
		if saveErr := saveRecovery(root, cfg.ProjectID, recovery); saveErr != nil {
			return result, otgerror.Wrap(otgerror.CodeRestoreFailed, "session recovery state could not be saved", errors.Join(err, saveErr))
		}
		result.Warnings = append(result.Warnings, "Native session recovery still needs attention: "+err.Error()+". Preserved at "+recovery.RecoveryDir)
		return result, nil
	}
	if previous.SnapshotID == recovery.SnapshotID {
		discovered, err := agentsessions.DiscoverWithOptions(root, roots, previous.Selection)
		if err != nil {
			return result, otgerror.Wrap(otgerror.CodeRestoreFailed, "sessions imported but session status could not be saved", err)
		}
		previous.SessionFingerprint = sessionFingerprint(discovered.Sessions)
		result.Warnings = append(result.Warnings, discovered.Warnings...)
		if err := saveBaseline(previous); err != nil {
			return result, otgerror.Wrap(otgerror.CodeRestoreFailed, "sessions imported but session receipt could not be saved", err)
		}
	}
	if err := clearRecovery(root, cfg.ProjectID, recovery); err != nil {
		return result, otgerror.Wrap(otgerror.CodeRestoreFailed, "sessions imported but recovery cleanup failed", err)
	}
	result.Sessions.Status = "recovered"
	return result, nil
}
