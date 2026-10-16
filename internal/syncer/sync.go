// Package syncer implements offline, authenticated handoff between user-owned
// machines. State lives outside repositories so restoring a repository cannot
// reset its conflict protection.
package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jbaehova/onthego/internal/agentsessions"
	"github.com/jbaehova/onthego/internal/capture"
	"github.com/jbaehova/onthego/internal/config"
	"github.com/jbaehova/onthego/internal/gitx"
	"github.com/jbaehova/onthego/internal/identity"
	"github.com/jbaehova/onthego/internal/otgerror"
	"github.com/jbaehova/onthego/internal/restore"
	"github.com/jbaehova/onthego/internal/snapshot"
)

type Result struct {
	Manifest snapshot.Manifest          `json:"manifest"`
	Root     string                     `json:"root"`
	Backup   string                     `json:"backup,omitempty"`
	Sessions agentsessions.ImportResult `json:"sessions"`
}

type StatusInfo struct {
	ProjectID   string `json:"project_id"`
	Root        string `json:"root"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	HasBaseline bool   `json:"has_baseline"`
	Changed     bool   `json:"changed"`
}

type baseline struct {
	SchemaVersion int       `json:"schema_version"`
	ProjectID     string    `json:"project_id"`
	Root          string    `json:"root"`
	SnapshotID    string    `json:"snapshot_id"`
	Ancestors     []string  `json:"ancestors,omitempty"`
	Fingerprint   string    `json:"fingerprint"`
	Include       []string  `json:"include,omitempty"`
	IncludeSecret []string  `json:"include_secret,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Export creates an encrypted, signed snapshot and records its exact source
// fingerprint as a pending transfer. Only MarkSynced acknowledges delivery.
func Export(ctx context.Context, root, output, recipient, version string, include, includeSecret []string) (snapshot.Manifest, error) {
	root, err := safeTarget(root)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	unlock, err := lock(root)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	defer unlock()
	cfg, err := config.Load(root)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	previous, err := readBaseline(root, cfg.ProjectID)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	keys, err := identity.LoadOrCreate()
	if err != nil {
		return snapshot.Manifest{}, err
	}
	var target age.Recipient
	if recipient == "" {
		target, err = keys.Recipient()
	} else {
		target, err = age.ParseX25519Recipient(recipient)
	}
	if err != nil {
		return snapshot.Manifest{}, err
	}
	roots, err := sessionRoots(cfg)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	dataRoot, err := config.ProjectDataRoot(cfg.ProjectID)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return snapshot.Manifest{}, err
	}
	contextDir, err := os.MkdirTemp(dataRoot, "sync-context-*")
	if err != nil {
		return snapshot.Manifest{}, err
	}
	defer os.RemoveAll(contextDir)
	options := effectiveOptions(root, cfg, previous, include, includeSecret)
	options.Output, options.ContextDir, options.Recipient, options.Keys, options.OnthegoVersion = output, contextDir, target, keys, version
	options.ParentSnapshotID = previous.SnapshotID
	options.ParentSnapshotIDs = previous.Ancestors
	projectBefore, err := capture.Fingerprint(ctx, options)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	native, err := agentsessions.Export(root, filepath.Join(contextDir, "native-sessions"), roots)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	before, err := combinedFingerprint(projectBefore, native.Sessions)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	created, err := capture.Create(ctx, options)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	after, err := fingerprint(ctx, options, roots)
	if err != nil || before != after {
		_ = os.Remove(output)
		return snapshot.Manifest{}, otgerror.Wrap(otgerror.CodeSourceChanged, "project or native agent sessions changed during capture; stop active agents and retry", err)
	}
	pending := baseline{SchemaVersion: 1, ProjectID: cfg.ProjectID, Root: root, SnapshotID: created.Manifest.SnapshotID, Ancestors: created.Manifest.ParentSnapshotIDs, Fingerprint: before, Include: options.Include, IncludeSecret: options.IncludeSecret, UpdatedAt: time.Now().UTC()}
	path, err := pendingPath(root, cfg.ProjectID, created.Manifest.SnapshotID)
	if err == nil {
		err = writeJSON(path, pending)
	}
	if err != nil {
		_ = os.Remove(output)
		return snapshot.Manifest{}, err
	}
	return created.Manifest, nil
}

// Preview uses the exact persisted selection policy used by Export, including
// secrets explicitly selected during earlier handoffs that still exist.
func Preview(ctx context.Context, root string, include, includeSecret []string) ([]capture.PreviewFile, error) {
	root, err := safeTarget(root)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	previous, err := readBaseline(root, cfg.ProjectID)
	if err != nil {
		return nil, err
	}
	return capture.Preview(ctx, effectiveOptions(root, cfg, previous, include, includeSecret))
}

// MarkSynced acknowledges a successfully delivered export. Its saved fingerprint
// is the exported state, even if work continued while transport was in progress.
func MarkSynced(ctx context.Context, root, snapshotID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := safeTarget(root)
	if err != nil {
		return err
	}
	unlock, err := lock(root)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	path, err := pendingPath(root, cfg.ProjectID, snapshotID)
	if err != nil {
		return err
	}
	var pending baseline
	if err := readJSON(path, &pending); err != nil {
		return fmt.Errorf("acknowledge snapshot: %w", err)
	}
	if err := validateBaseline(pending, root, cfg.ProjectID); err != nil {
		return err
	}
	if pending.SnapshotID != snapshotID {
		return errors.New("pending snapshot ID mismatch")
	}
	current, err := readBaseline(root, cfg.ProjectID)
	if err != nil {
		return err
	}
	if current.SnapshotID != "" && current.UpdatedAt.After(pending.UpdatedAt) {
		return otgerror.New(otgerror.CodeGenerationConflict, "a newer handoff was applied or acknowledged before this transfer completed")
	}
	if err := saveBaseline(pending); err != nil {
		return err
	}
	return os.Remove(path)
}

// Status reports modifications since the last acknowledged export or import,
// including added/changed native sessions, refs, staged, and unstaged files.
func Status(ctx context.Context, root string) (StatusInfo, error) {
	root, err := safeTarget(root)
	if err != nil {
		return StatusInfo{}, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return StatusInfo{}, err
	}
	previous, err := readBaseline(root, cfg.ProjectID)
	if err != nil {
		return StatusInfo{}, err
	}
	result := StatusInfo{ProjectID: cfg.ProjectID, Root: root, SnapshotID: previous.SnapshotID, HasBaseline: previous.SnapshotID != "", Changed: true}
	if !result.HasBaseline {
		return result, nil
	}
	roots, err := sessionRoots(cfg)
	if err != nil {
		return StatusInfo{}, err
	}
	current, err := fingerprint(ctx, optionsFor(root, cfg, previous), roots)
	if err != nil {
		return StatusInfo{}, err
	}
	result.Changed = current != previous.Fingerprint
	return result, nil
}

// Apply restores to a sibling stage, validates project/session identity, then
// replaces the destination. Every replaced tree is retained as a sibling backup.
// Existing work needs an unchanged baseline, unless replace explicitly overrides
// the conflict. A different project can never be overwritten, even with replace.
func Apply(ctx context.Context, packagePath, root, trustedSigner string, replace bool) (Result, error) {
	if trustedSigner == "" {
		return Result{}, otgerror.New(otgerror.CodeTargetAuth, "a trusted sender signing key is required")
	}
	if _, err := identity.ParsePublic(trustedSigner); err != nil {
		return Result{}, err
	}
	root, err := safeTarget(root)
	if err != nil {
		return Result{}, err
	}
	unlock, err := lock(root)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	previous, before, existing, err := inspectDestination(ctx, root, replace)
	if err != nil {
		return Result{}, err
	}
	keys, err := identity.LoadOrCreate()
	if err != nil {
		return Result{}, err
	}
	secret, err := keys.Identity()
	if err != nil {
		return Result{}, err
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return Result{}, err
	}
	if _, err := safeTarget(root); err != nil {
		return Result{}, err
	}
	stage, err := os.MkdirTemp(parent, ".onthego-stage-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(stage)
	stageRoot := filepath.Join(stage, "project")
	restored, err := restore.Package(ctx, packagePath, stageRoot, secret, trustedSigner)
	if err != nil {
		return Result{}, err
	}
	cfg, err := config.Load(stageRoot)
	if err != nil {
		return Result{}, fmt.Errorf("snapshot project configuration: %w", err)
	}
	if cfg.ProjectID != restored.Manifest.ProjectID {
		return Result{}, otgerror.New(otgerror.CodePackageInvalid, "snapshot manifest and project configuration IDs differ")
	}
	if existing && previous.ProjectID != cfg.ProjectID {
		return Result{}, otgerror.New(otgerror.CodeGenerationConflict, "destination belongs to a different project")
	}
	if existing && !replace && previous.SnapshotID != restored.Manifest.SnapshotID && !contains(restored.Manifest.ParentSnapshotIDs, previous.SnapshotID) {
		return Result{}, otgerror.New(otgerror.CodeGenerationConflict, "incoming snapshot does not descend from the destination's last handoff; synchronize from the current machine or use --replace after reviewing its backup")
	}
	roots, err := sessionRoots(cfg)
	if err != nil {
		return Result{}, err
	}
	native := filepath.Join(stageRoot, ".onthego.local", "context", "native-sessions")
	if _, err := agentsessions.ValidateImport(native, root, roots); err != nil {
		return Result{}, fmt.Errorf("validate native agent sessions: %w", err)
	}
	var localFiles []localFile
	if existing {
		localFiles, err = preserveLocalFiles(ctx, root, stageRoot, previous, cfg)
		if err != nil {
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if _, err := safeTarget(root); err != nil {
		return Result{}, err
	}
	if existing {
		current, err := config.Load(root)
		if err != nil {
			return Result{}, err
		}
		if current.ProjectID != previous.ProjectID {
			return Result{}, otgerror.New(otgerror.CodeSourceChanged, "destination project identity changed during restore")
		}
		currentRoots, err := sessionRoots(current)
		if err != nil {
			return Result{}, err
		}
		after, err := fingerprint(ctx, optionsFor(root, current, previous), currentRoots)
		if err != nil {
			return Result{}, err
		}
		if before != after {
			return Result{}, otgerror.New(otgerror.CodeSourceChanged, "destination or agent sessions changed while the incoming snapshot was being restored")
		}
		if err := verifyLocalFiles(root, localFiles); err != nil {
			return Result{}, err
		}
	} else if _, err := os.Lstat(root); err == nil || !os.IsNotExist(err) {
		return Result{}, otgerror.Wrap(otgerror.CodeSourceChanged, "destination appeared while the incoming snapshot was being restored", err)
	}
	result := Result{Manifest: restored.Manifest, Root: root}
	if existing {
		backup, err := os.MkdirTemp(parent, ".onthego-backup-"+filepath.Base(root)+"-*")
		if err != nil {
			return Result{}, err
		}
		if err := os.Remove(backup); err != nil {
			return Result{}, err
		}
		if err := os.Rename(root, backup); err != nil {
			return Result{}, err
		}
		result.Backup = backup
	}
	if err := os.Rename(stageRoot, root); err != nil {
		if result.Backup != "" {
			err = errors.Join(err, os.Rename(result.Backup, root))
		}
		return result, err
	}
	native = filepath.Join(root, ".onthego.local", "context", "native-sessions")
	result.Sessions, err = agentsessions.Import(native, root, roots)
	if err != nil {
		rollback := os.Rename(root, stageRoot)
		if rollback == nil && result.Backup != "" {
			rollback = os.Rename(result.Backup, root)
		}
		return result, errors.Join(fmt.Errorf("import native agent sessions: %w", err), rollback)
	}
	next := baseline{SchemaVersion: 1, Root: root, ProjectID: cfg.ProjectID, SnapshotID: restored.Manifest.SnapshotID, Ancestors: restored.Manifest.ParentSnapshotIDs, UpdatedAt: time.Now().UTC()}
	for _, file := range restored.Manifest.Files {
		if file.Secret {
			next.IncludeSecret = append(next.IncludeSecret, file.RelativePath)
		} else if file.PolicySource == "include" {
			next.Include = append(next.Include, file.RelativePath)
		}
	}
	next.Include, next.IncludeSecret = unique(next.Include), unique(next.IncludeSecret)
	next.Fingerprint, err = fingerprint(ctx, optionsFor(root, cfg, next), roots)
	if err == nil {
		err = saveBaseline(next)
	}
	if err != nil {
		return result, &otgerror.Error{Code: otgerror.CodeRestoreFailed, Message: "snapshot applied, but saving its synchronization baseline failed", Preserved: result.Backup, Cause: err}
	}
	return result, nil
}

func inspectDestination(ctx context.Context, root string, replace bool) (baseline, string, bool, error) {
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return baseline{}, "", false, nil
	}
	if err != nil {
		return baseline{}, "", false, err
	}
	if !info.IsDir() {
		return baseline{}, "", true, otgerror.New(otgerror.CodeInput, "destination must be a directory")
	}
	gitInfo, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil || !gitInfo.IsDir() || gitInfo.Mode()&os.ModeSymlink != 0 {
		return baseline{}, "", true, otgerror.New(otgerror.CodeUnsupportedGit, "destination must be a standalone repository; linked worktrees cannot be replaced")
	}
	worktrees, err := gitx.Run(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return baseline{}, "", true, err
	}
	if strings.Count(string(worktrees.Stdout), "worktree ") != 1 {
		return baseline{}, "", true, otgerror.New(otgerror.CodeUnsupportedGit, "repository has linked worktrees; replacing its Git directory would break them")
	}
	cfg, err := config.Load(root)
	if err != nil {
		return baseline{}, "", true, fmt.Errorf("existing destination must have a matching onthego project configuration: %w", err)
	}
	previous, err := readBaseline(root, cfg.ProjectID)
	if err != nil {
		return baseline{}, "", true, err
	}
	previous.ProjectID, previous.Root = cfg.ProjectID, root
	if previous.SnapshotID == "" && !replace {
		return baseline{}, "", true, otgerror.New(otgerror.CodeGenerationConflict, "existing destination has no synchronization baseline; use --replace to retain a backup and replace it")
	}
	roots, err := sessionRoots(cfg)
	if err != nil {
		return baseline{}, "", true, err
	}
	current, err := fingerprint(ctx, optionsFor(root, cfg, previous), roots)
	if err != nil {
		return baseline{}, "", true, err
	}
	if !replace && current != previous.Fingerprint {
		return baseline{}, "", true, otgerror.New(otgerror.CodeGenerationConflict, "destination has local file or agent session changes since its last handoff; send those changes first or use --replace to retain a backup and replace them")
	}
	return previous, current, true, nil
}

func optionsFor(root string, cfg config.Config, state baseline) capture.Options {
	return capture.Options{Root: root, Config: cfg, Include: state.Include, IncludeSecret: existingPaths(root, state.IncludeSecret)}
}

func effectiveOptions(root string, cfg config.Config, state baseline, include, includeSecret []string) capture.Options {
	options := optionsFor(root, cfg, state)
	options.Include = unique(append(append([]string{}, options.Include...), include...))
	options.IncludeSecret = unique(append(options.IncludeSecret, includeSecret...))
	return options
}

func sessionRoots(cfg config.Config) (agentsessions.Roots, error) {
	roots, err := agentsessions.DefaultRoots()
	if cfg.Agent == "none" {
		roots = agentsessions.Roots{}
	}
	if cfg.Agent == "codex" {
		roots.ClaudeHome = ""
	}
	if cfg.Agent == "claude" {
		roots.CodexHome = ""
	}
	return roots, err
}

func fingerprint(ctx context.Context, options capture.Options, roots agentsessions.Roots) (string, error) {
	project, err := capture.Fingerprint(ctx, options)
	if err != nil {
		return "", err
	}
	sessions, err := agentsessions.Discover(options.Root, roots)
	if err != nil {
		return "", err
	}
	return combinedFingerprint(project, sessions)
}

func combinedFingerprint(project string, sessions []agentsessions.Session) (string, error) {
	// Normalize nil/empty collections and ordering so Export and Discover agree.
	normalized := append([]agentsessions.Session{}, sessions...)
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].Agent+"/"+normalized[i].Path < normalized[j].Agent+"/"+normalized[j].Path
	})
	data, err := json.Marshal(struct {
		Project  string
		Sessions []agentsessions.Session
	}{project, normalized})
	if err != nil {
		return "", err
	}
	return digest(data), nil
}

func existingPaths(root string, paths []string) []string {
	var found []string
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
			found = append(found, path)
		}
	}
	return found
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
func unique(values []string) []string {
	result := []string{}
	for _, value := range values {
		if !contains(result, value) {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
