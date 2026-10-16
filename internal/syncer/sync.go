// Package syncer coordinates authenticated handoffs between user-owned devices.
// Workspace conflict checks are independent of native session conflict handling.
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
	"time"

	"filippo.io/age"
	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/capture"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/packageotg"
	"github.com/peerproblem/onthego/internal/restore"
	"github.com/peerproblem/onthego/internal/snapshot"
)

type Result struct {
	Manifest       snapshot.Manifest          `json:"manifest"`
	Root           string                     `json:"root"`
	Backup         string                     `json:"backup,omitempty"`
	Sessions       agentsessions.ImportResult `json:"sessions"`
	AlreadyApplied bool                       `json:"already_applied,omitempty"`
	Warnings       []string                   `json:"warnings,omitempty"`
}
type StatusInfo struct {
	ProjectID    string    `json:"project_id"`
	Root         string    `json:"root"`
	SnapshotID   string    `json:"snapshot_id,omitempty"`
	HasBaseline  bool      `json:"has_baseline"`
	Changed      bool      `json:"changed"`
	GitTips      []string  `json:"git_tips,omitempty"`
	WIPCommit    string    `json:"wip_commit,omitempty"`
	ActiveDevice string    `json:"active_device,omitempty"`
	HandedOffAt  time.Time `json:"handed_off_at,omitempty"`
	Warnings     []string  `json:"warnings,omitempty"`
}
type baseline struct {
	SchemaVersion      int                     `json:"schema_version"`
	ProjectID          string                  `json:"project_id"`
	Root               string                  `json:"root"`
	SnapshotID         string                  `json:"snapshot_id"`
	WIPCommit          string                  `json:"wip_commit"`
	Fingerprint        string                  `json:"fingerprint"`
	SessionFingerprint string                  `json:"session_fingerprint,omitempty"`
	Selection          agentsessions.Selection `json:"selection"`
	Include            []string                `json:"include,omitempty"`
	IncludeSecret      []string                `json:"include_secret,omitempty"`
	Managed            []string                `json:"managed,omitempty"`
	Peer               string                  `json:"peer,omitempty"`
	ActiveDevice       string                  `json:"active_device,omitempty"`
	HandedOffAt        time.Time               `json:"handed_off_at,omitempty"`
	UpdatedAt          time.Time               `json:"updated_at"`
}
type ExportOptions struct {
	Include       []string
	IncludeSecret []string
	Peer          string
	RecipientTips []string
	Selection     agentsessions.Selection
}
type ApplyOptions struct {
	Replace  bool
	Peer     string
	Sessions agentsessions.ImportOptions
}

func Export(ctx context.Context, root, output, recipient, version string, include, includeSecret []string) (snapshot.Manifest, error) {
	return ExportWithOptions(ctx, root, output, recipient, version, ExportOptions{Include: include, IncludeSecret: includeSecret})
}
func ExportWithOptions(ctx context.Context, root, output, recipient, version string, opts ExportOptions) (snapshot.Manifest, error) {
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
	if _, err := pendingBaselines(root, cfg.ProjectID); err != nil {
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
	home, err := config.DataRoot()
	if err != nil {
		return snapshot.Manifest{}, err
	}
	contextDir, err := fsx.TempDir(home, "context-")
	if err != nil {
		return snapshot.Manifest{}, err
	}
	defer fsx.ReleaseTemp(contextDir)
	native, err := agentsessions.ExportWithOptions(root, filepath.Join(contextDir, "native-sessions"), roots, opts.Selection)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	options := effectiveOptions(root, cfg, previous, opts.Include, opts.IncludeSecret)
	options.Output, options.ContextDir, options.Recipient, options.Keys, options.OnthegoVersion = output, contextDir, target, keys, version
	options.ParentSnapshotID = previous.SnapshotID
	options.ParentWIPCommit = previous.WIPCommit
	options.RecipientTips = opts.RecipientTips
	created, err := capture.Create(ctx, options)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	// Session metadata hashes are cached by file identity, size, and mtime. This
	// stability check does not reread every unchanged transcript.
	after, err := agentsessions.DiscoverWithOptions(root, roots, opts.Selection)
	if err != nil || sessionFingerprint(native.Sessions) != sessionFingerprint(after.Sessions) {
		_ = os.Remove(output)
		return snapshot.Manifest{}, otgerror.Wrap(otgerror.CodeSourceChanged, "native agent sessions changed during capture; stop active agents and retry", err)
	}
	pending := baseline{SchemaVersion: 2, ProjectID: cfg.ProjectID, Root: root, SnapshotID: created.Manifest.SnapshotID, WIPCommit: created.Manifest.Git.WIPCommit, Fingerprint: created.Fingerprint, SessionFingerprint: sessionFingerprint(native.Sessions), Selection: opts.Selection, Include: options.Include, IncludeSecret: options.IncludeSecret, Peer: opts.Peer, UpdatedAt: time.Now().UTC()}
	for _, file := range created.Manifest.Files {
		pending.Managed = append(pending.Managed, file.RelativePath)
	}
	path, err := pendingPath(root, cfg.ProjectID, pending.SnapshotID)
	if err == nil {
		err = writeJSON(path, pending)
	}
	if err == nil {
		_, err = pendingBaselines(root, cfg.ProjectID)
	}
	if err != nil {
		_ = os.Remove(output)
		return snapshot.Manifest{}, err
	}
	return created.Manifest, nil
}
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
func MarkSynced(ctx context.Context, root, snapshotID string) error {
	return MarkSyncedPeer(ctx, root, snapshotID, "")
}
func MarkSyncedPeer(ctx context.Context, root, snapshotID, peer string) error {
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
	return acknowledge(root, cfg.ProjectID, snapshotID, peer)
}
func acknowledge(root, projectID, snapshotID, peer string) error {
	path, err := pendingPath(root, projectID, snapshotID)
	if err != nil {
		return err
	}
	current, err := readBaseline(root, projectID)
	if err != nil {
		return err
	}
	var pending baseline
	if err := readJSON(path, &pending); err != nil {
		if os.IsNotExist(err) && current.SnapshotID == snapshotID {
			return nil
		}
		return fmt.Errorf("acknowledge snapshot: %w", err)
	}
	if err := validateBaseline(pending, root, projectID); err != nil {
		return err
	}
	if pending.SnapshotID != snapshotID {
		return errors.New("pending snapshot ID mismatch")
	}
	if current.SnapshotID != "" && current.UpdatedAt.After(pending.UpdatedAt) {
		// Keep this peer's delivery receipt without moving the current workspace
		// baseline backwards if another handoff completed during transport.
		if peer != "" {
			pending.Peer = peer
		}
		receipt, err := peerStatePath(root, projectID, pending.Peer)
		if err != nil {
			return err
		}
		if err := writeJSON(receipt, pending); err != nil {
			return err
		}
		return os.Remove(path)
	}
	if peer != "" {
		pending.Peer = peer
	}
	pending.ActiveDevice = pending.Peer
	if pending.ActiveDevice != "" {
		pending.HandedOffAt = time.Now().UTC()
	}
	if err := saveBaseline(pending); err != nil {
		return err
	}
	return os.Remove(path)
}

// ReconcilePending recovers a lost acknowledgment from the authenticated
// recipient's status. A foreign/unknown snapshot never advances local state.
func ReconcilePending(ctx context.Context, root, snapshotID, peer string) error {
	if snapshotID == "" {
		return nil
	}
	root, err := safeTarget(root)
	if err != nil {
		return err
	}
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
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if pending.Peer != "" && pending.Peer != peer {
		return nil
	}
	return MarkSyncedPeer(ctx, root, snapshotID, peer)
}
func Status(ctx context.Context, root string) (StatusInfo, error) {
	root, err := safeTarget(root)
	if err != nil {
		return StatusInfo{}, err
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return StatusInfo{Root: root}, nil
	} else if err != nil {
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
	result := StatusInfo{ProjectID: cfg.ProjectID, Root: root, SnapshotID: previous.SnapshotID, HasBaseline: previous.SnapshotID != "", Changed: true, WIPCommit: previous.WIPCommit, ActiveDevice: previous.ActiveDevice, HandedOffAt: previous.HandedOffAt}
	result.GitTips, err = gitx.Tips(ctx, root)
	if err != nil {
		return StatusInfo{}, err
	}
	if result.ActiveDevice != "" {
		result.Warnings = append(result.Warnings, fmt.Sprintf("This workspace was handed to %s at %s. Pull before editing or resuming its agent sessions.", result.ActiveDevice, result.HandedOffAt.Format(time.RFC3339)))
	}
	if !result.HasBaseline {
		return result, nil
	}
	workspace, err := capture.Fingerprint(ctx, optionsFor(root, cfg, previous))
	if err != nil {
		return StatusInfo{}, err
	}
	roots, err := sessionRoots(cfg)
	if err != nil {
		return StatusInfo{}, err
	}
	sessions, err := agentsessions.DiscoverWithOptions(root, roots, previous.Selection)
	if err != nil {
		result.Changed = workspace != previous.Fingerprint
		result.Warnings = append(result.Warnings, "Native session status is unavailable: "+err.Error())
		return result, nil
	}
	result.Warnings = append(result.Warnings, sessions.Warnings...)
	result.Changed = workspace != previous.Fingerprint || sessionFingerprint(sessions.Sessions) != previous.SessionFingerprint
	return result, nil
}
func Apply(ctx context.Context, packagePath, root, trustedSigner string, replace bool) (Result, error) {
	return ApplyWithOptions(ctx, packagePath, root, trustedSigner, ApplyOptions{Replace: replace})
}
func ApplyWithOptions(ctx context.Context, packagePath, root, trustedSigner string, opts ApplyOptions) (Result, error) {
	switch opts.Sessions.ConflictPolicy {
	case "", "skip", "theirs", "keep-both":
	default:
		return Result{}, otgerror.New(otgerror.CodeInput, "session policy must be skip, theirs, or keep-both")
	}
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
	keys, err := identity.LoadOrCreate()
	if err != nil {
		return Result{}, err
	}
	secret, err := keys.Identity()
	if err != nil {
		return Result{}, err
	}
	home, err := config.DataRoot()
	if err != nil {
		return Result{}, err
	}
	stage, err := fsx.TempDir(home, "incoming-")
	if err != nil {
		return Result{}, err
	}
	defer fsx.ReleaseTemp(stage)
	extracted := filepath.Join(stage, "payload")
	manifest, err := packageotg.ExtractContext(ctx, packagePath, extracted, secret, trustedSigner)
	if err != nil {
		return Result{}, err
	}
	previous, before, existing, err := inspectDestination(ctx, root, true)
	if err != nil {
		return Result{}, err
	}
	if existing && previous.ProjectID != manifest.ProjectID {
		return Result{}, otgerror.New(otgerror.CodeGenerationConflict, "destination belongs to a different project")
	}
	// Idempotency must be checked before the dirty-workspace gate: replaying an
	// acknowledged package must never clobber edits made after that application.
	if previous.SnapshotID == manifest.SnapshotID {
		return Result{Manifest: manifest, Root: root, AlreadyApplied: true}, nil
	}
	cfg, err := restore.Config(ctx, extracted, manifest, root)
	if err != nil {
		return Result{}, err
	}
	if cfg.ProjectID != manifest.ProjectID {
		return Result{}, otgerror.New(otgerror.CodePackageInvalid, "snapshot manifest and project configuration IDs differ")
	}
	if existing && !opts.Replace {
		if err := protectOtherBranch(ctx, root, manifest); err != nil {
			return Result{}, err
		}
		previous, before, err = acceptedBaseline(ctx, root, cfg.ProjectID, previous, before, manifest)
		if err != nil {
			return Result{}, err
		}
	}
	roots, err := sessionRoots(cfg)
	if err != nil {
		return Result{}, err
	}
	native := filepath.Join(extracted, "context", "native-sessions")
	hasSessions := false
	if _, err := os.Stat(native); err == nil {
		hasSessions = true
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	// The package already authenticates every session byte and archive path.
	// Adapter validation runs at import time so a receiver-local adapter failure
	// cannot prevent workspace delivery; failed imports are preserved privately.
	if opts.Sessions.ConflictsDir == "" {
		opts.Sessions.ConflictsDir = filepath.Join(home, "conflicts")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if existing {
		localCfg, err := config.Load(root)
		if err != nil {
			return Result{}, err
		}
		current, err := capture.Fingerprint(ctx, optionsFor(root, localCfg, previous))
		if err != nil {
			return Result{}, err
		}
		if current != before {
			return Result{}, otgerror.New(otgerror.CodeSourceChanged, "destination changed while the incoming snapshot was being verified")
		}
	} else if _, err := os.Lstat(root); err == nil || !os.IsNotExist(err) {
		return Result{}, otgerror.Wrap(otgerror.CodeSourceChanged, "destination appeared during handoff", err)
	}
	restored, err := restore.Apply(ctx, extracted, root, manifest, previous.Managed, previous.IncludeSecret)
	if err != nil {
		return Result{}, err
	}
	result := Result{Manifest: manifest, Root: root, Backup: restored.Backup}
	if hasSessions {
		result.Sessions, err = agentsessions.ImportWithOptions(native, root, roots, opts.Sessions)
		if err != nil {
			// Workspace delivery remains usable if an adapter cannot write its home.
			// Its authenticated inbound logs are retained privately for explicit retry.
			preserved, keepErr := os.MkdirTemp(home, "session-recovery-"+manifest.SnapshotID[:12]+"-")
			if keepErr == nil {
				keepErr = preserveSessionBundle(native, preserved)
			}
			if keepErr != nil {
				return result, &otgerror.Error{Code: otgerror.CodeRestoreFailed, Message: "workspace applied but session recovery could not be preserved", Preserved: result.Backup, Cause: errors.Join(err, keepErr)}
			}
			result.Warnings = append(result.Warnings, "Workspace applied; native session import needs attention: "+err.Error()+". Preserved at "+preserved)
		}
	}
	next := baseline{SchemaVersion: 2, Root: root, ProjectID: cfg.ProjectID, SnapshotID: manifest.SnapshotID, WIPCommit: manifest.Git.WIPCommit, Peer: opts.Peer, UpdatedAt: time.Now().UTC()}
	for _, file := range manifest.Files {
		next.Managed = append(next.Managed, file.RelativePath)
		if file.Secret {
			next.IncludeSecret = append(next.IncludeSecret, file.RelativePath)
		} else if file.PolicySource == "include" {
			next.Include = append(next.Include, file.RelativePath)
		}
	}
	next.Include, next.IncludeSecret = unique(next.Include), unique(next.IncludeSecret)
	next.Fingerprint, err = capture.Fingerprint(ctx, optionsFor(root, cfg, next))
	if err == nil {
		sessions, discoverErr := agentsessions.DiscoverWithOptions(root, roots, next.Selection)
		if discoverErr != nil {
			result.Warnings = append(result.Warnings, "Workspace receipt saved without session status: "+discoverErr.Error())
		} else {
			next.SessionFingerprint = sessionFingerprint(sessions.Sessions)
		}
	}
	if err == nil {
		err = saveBaseline(next)
	}
	if err != nil {
		return result, &otgerror.Error{Code: otgerror.CodeRestoreFailed, Message: "snapshot applied, but saving its handoff baseline failed", Preserved: result.Backup, Cause: err}
	}
	return result, nil
}

// Switching branches must not silently replace independent work held by an
// existing noncurrent branch. Explicit replacement uses restore's ref backup.
func protectOtherBranch(ctx context.Context, root string, manifest snapshot.Manifest) error {
	branch := manifest.SourceBranch
	if branch == "" || branch == "(detached)" {
		return nil
	}
	current, _ := gitx.Text(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if current == branch {
		return nil
	}
	local, err := gitx.Text(ctx, root, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return nil
	}
	compatible, err := gitx.IsAncestor(ctx, root, local, manifest.SourceGitHead)
	if err != nil {
		return err
	}
	if !compatible {
		return otgerror.New(otgerror.CodeGenerationConflict, "destination branch "+branch+" has independent commits; reconcile it with Git or use --replace to retain its prior ref in the backup")
	}
	return nil
}

func acceptedBaseline(ctx context.Context, root, projectID string, previous baseline, initialFingerprint string, manifest snapshot.Manifest) (baseline, string, error) {
	candidates, err := pendingBaselines(root, projectID)
	if err != nil {
		return baseline{}, "", err
	}
	if previous.SnapshotID != "" {
		candidates = append([]baseline{previous}, candidates...)
	}
	cfg, err := config.Load(root)
	if err != nil {
		return baseline{}, "", err
	}
	policyKey := func(state baseline) string {
		data, _ := json.Marshal([][]string{state.Include, state.IncludeSecret})
		return string(data)
	}
	fingerprints := map[string]string{policyKey(previous): initialFingerprint}
	for _, candidate := range candidates {
		if candidate.WIPCommit == "" {
			continue
		}
		ok, err := gitx.IsAncestor(ctx, root, candidate.WIPCommit, manifest.Git.WIPCommit)
		if err != nil {
			return baseline{}, "", err
		}
		if !ok {
			continue
		}
		key := policyKey(candidate)
		fp, known := fingerprints[key]
		if !known {
			fp, err = capture.Fingerprint(ctx, optionsFor(root, cfg, candidate))
			if err != nil {
				return baseline{}, "", err
			}
			fingerprints[key] = fp
		}
		if fp == candidate.Fingerprint {
			return candidate, fp, nil
		}
	}
	return baseline{}, "", otgerror.New(otgerror.CodeGenerationConflict, "incoming handoff diverges from the destination or local workspace changed; send those changes first or use --replace after review")
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
	if err := gitx.Validate(ctx, root); err != nil {
		return baseline{}, "", true, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return baseline{}, "", true, fmt.Errorf("existing destination requires project configuration: %w", err)
	}
	previous, err := readBaseline(root, cfg.ProjectID)
	if err != nil {
		return baseline{}, "", true, err
	}
	previous.ProjectID, previous.Root = cfg.ProjectID, root
	current, err := capture.Fingerprint(ctx, optionsFor(root, cfg, previous))
	if err != nil {
		return baseline{}, "", true, err
	}
	if !replace && (previous.SnapshotID == "" || current != previous.Fingerprint) {
		return baseline{}, "", true, otgerror.New(otgerror.CodeGenerationConflict, "destination has no unchanged handoff baseline")
	}
	return previous, current, true, nil
}
func Undo(ctx context.Context, root, ref string) (restore.Result, error) {
	root, err := safeTarget(root)
	if err != nil {
		return restore.Result{}, err
	}
	unlock, err := lock(root)
	if err != nil {
		return restore.Result{}, err
	}
	defer unlock()
	cfg, err := config.Load(root)
	if err != nil {
		return restore.Result{}, err
	}
	result, err := restore.Undo(ctx, root, ref)
	if err != nil {
		return result, err
	}
	// Undo restores work, not proof that another peer has received it.
	path, err := statePath(root, cfg.ProjectID)
	if err != nil {
		return result, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return result, err
	}
	return result, nil
}
func preserveSessionBundle(source, target string) error {
	if err := checkStatePath(target); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0o700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("session recovery contains an unsupported file")
		}
		if _, err := os.Lstat(dest); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		return fsx.CopyRegular(path, dest, 0o600)
	})
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
func sessionFingerprint(sessions []agentsessions.Session) string {
	normalized := append([]agentsessions.Session{}, sessions...)
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].Agent+"/"+normalized[i].Path < normalized[j].Agent+"/"+normalized[j].Path
	})
	data, _ := json.Marshal(normalized)
	return digest(data)
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
