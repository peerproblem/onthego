// Package agentsessions transfers project-scoped native agent histories. It never
// copies agent authentication, global settings, SQLite databases, or live processes.
package agentsessions

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/otgerror"
)

const (
	SchemaVersion   = 1
	MaxSessionBytes = 256 << 20
	MaxBundleBytes  = 1 << 30
)

type Roots struct {
	CodexHome  string
	ClaudeHome string
}

type Session struct {
	Agent      string `json:"agent"`
	ID         string `json:"id"`
	Path       string `json:"path"`
	CWD        string `json:"cwd"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	LastActive string `json:"last_active,omitempty"`
}

type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	SourceRoot    string            `json:"source_root"`
	SourceAliases map[string]string `json:"source_aliases,omitempty"`
	Sessions      []Session         `json:"sessions"`
	Warnings      []string          `json:"warnings,omitempty"`
}

type ImportResult struct {
	Status         string    `json:"status,omitempty"`
	Imported       int       `json:"imported"`
	Updated        int       `json:"updated"`
	Skipped        int       `json:"skipped"`
	Conflicts      int       `json:"conflicts"`
	PreservedPaths []string  `json:"preserved_paths,omitempty"`
	Sessions       []Session `json:"sessions,omitempty"`
	Warnings       []string  `json:"warnings,omitempty"`
}

func DefaultRoots() (Roots, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Roots{}, err
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	return Roots{CodexHome: codex, ClaudeHome: claude}, nil
}

var warnings = []string{
	"Stop agents before handoff; running processes are not transferred.",
	"Native histories are copied in full and may contain secrets from conversation or tool output; authentication files are excluded.",
	"Codex desktop sidebar state, SQLite databases, global history indexes, and agent settings are not transferred. Resume imported JSONL sessions with the agent CLI; compatibility depends on the installed agent version.",
}

// Export writes a portable manifest and full native logs to a new directory.
// The caller must transport this sensitive directory over an encrypted channel.
func Export(projectRoot, bundleDir string, roots Roots) (Manifest, error) {
	return ExportWithOptions(projectRoot, bundleDir, roots, Selection{})
}

func ExportWithOptions(projectRoot, bundleDir string, roots Roots, selection Selection) (Manifest, error) {
	if err := ValidateSelection(selection); err != nil {
		return Manifest{}, err
	}
	if selection.None {
		return Manifest{SchemaVersion: SchemaVersion, Sessions: []Session{}}, nil
	}
	root, err := fsx.Canonical(projectRoot)
	if err != nil {
		return Manifest{}, err
	}
	files, discoveryWarnings, err := discover(root, roots, selection, true)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{SchemaVersion: SchemaVersion, SourceRoot: root, SourceAliases: map[string]string{}, Sessions: []Session{}, Warnings: append(append([]string(nil), warnings...), discoveryWarnings...)}
	if _, err := os.Lstat(bundleDir); err == nil {
		return Manifest{}, errors.New("native session export directory already exists")
	} else if !os.IsNotExist(err) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(filepath.Dir(bundleDir), 0o700); err != nil {
		return Manifest{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(bundleDir), ".sessions-*")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(staging)
	for _, file := range files {
		collectAliases(file.data, root, manifest.SourceAliases)
		manifest.Sessions = append(manifest.Sessions, file.session)
		target := filepath.Join(staging, file.session.Agent, filepath.FromSlash(file.session.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return Manifest{}, err
		}
		if err := os.WriteFile(target, file.data, 0o600); err != nil {
			return Manifest{}, err
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), data, 0o600); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(staging, bundleDir); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

type writePlan struct {
	home, relative     string
	incoming, previous []byte
	exists             bool
}

// ValidateImport checks every hash, path, and destination collision without
// modifying the destination. Import repeats these checks under its own lock.
func ValidateImport(bundleDir, projectRoot string, roots Roots) (ImportResult, error) {
	return ValidateImportWithOptions(bundleDir, projectRoot, roots, ImportOptions{})
}

func ValidateImportWithOptions(bundleDir, projectRoot string, roots Roots, options ImportOptions) (ImportResult, error) {
	_, result, err := planImport(bundleDir, projectRoot, roots, options)
	return result, err
}

func planImport(bundleDir, projectRoot string, roots Roots, options ImportOptions) ([]writePlan, ImportResult, error) {
	manifest, err := readManifest(bundleDir)
	if err != nil {
		return nil, ImportResult{}, err
	}
	return planImportManifest(bundleDir, projectRoot, roots, options, manifest)
}

func planImportManifest(bundleDir, projectRoot string, roots Roots, options ImportOptions, manifest Manifest) ([]writePlan, ImportResult, error) {
	if options.ConflictPolicy == "" {
		options.ConflictPolicy = "keep-both"
	}
	if options.ConflictPolicy != "skip" && options.ConflictPolicy != "theirs" && options.ConflictPolicy != "keep-both" {
		return nil, ImportResult{}, otgerror.New(otgerror.CodeInput, "session conflict policy must be skip, theirs, or keep-both")
	}
	root, err := fsx.Canonical(projectRoot)
	if err != nil {
		return nil, ImportResult{}, err
	}
	result := ImportResult{Warnings: append([]string(nil), warnings...)}
	for _, warning := range manifest.Warnings {
		duplicate := false
		for _, existing := range result.Warnings {
			if existing == warning {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result.Warnings = append(result.Warnings, warning)
		}
	}
	var plans []writePlan
	seen := map[string]bool{}
	archives := map[string]bool{}
	var total int64
	for _, session := range manifest.Sessions {
		if err := validatePath(session.Agent, session.Path); err != nil {
			return nil, result, err
		}
		if !within(aliasPath(session.CWD, manifest.SourceAliases), manifest.SourceRoot) {
			return nil, result, errors.New("native session manifest contains unrelated project")
		}
		source := filepath.Join(session.Agent, filepath.FromSlash(session.Path))
		if err := noSymlinks(bundleDir, source); err != nil {
			return nil, result, err
		}
		original, err := readStable(filepath.Join(bundleDir, source), MaxSessionBytes)
		if err != nil {
			return nil, result, err
		}
		total += int64(len(original))
		if total > MaxBundleBytes {
			return nil, result, errors.New("native session bundle exceeds 1 GiB")
		}
		if digest(original) != session.SHA256 || int64(len(original)) != session.SizeBytes {
			return nil, result, errors.New("native session content does not match manifest")
		}
		id, cwd, err := metadata(original, session.Agent)
		if err != nil || id != session.ID || cwd != session.CWD {
			return nil, result, errors.New("native session metadata does not match manifest")
		}
		incoming, err := rewriteAliases(original, manifest.SourceRoot, root, manifest.SourceAliases)
		if err != nil {
			return nil, result, err
		}
		home, relative := roots.CodexHome, session.Path
		if session.Agent == "claude" {
			home = roots.ClaudeHome
			parts := strings.Split(session.Path, "/")
			relative = "projects/" + ProjectDirectory(remap(aliasPath(session.CWD, manifest.SourceAliases), manifest.SourceRoot, root)) + "/" + strings.Join(parts[2:], "/")
		}
		if home == "" {
			return nil, result, otgerror.New(otgerror.CodeInput, fmt.Sprintf("%s home is required to import captured sessions", session.Agent))
		}
		home, err = absolute(home)
		if err != nil {
			return nil, result, err
		}
		key := filepath.Join(home, filepath.FromSlash(relative))
		if seen[key] {
			return nil, result, fmt.Errorf("duplicate native session destination: %s", key)
		}
		seen[key] = true
		if err := noSymlinks(home, relative); err != nil {
			return nil, result, err
		}
		previous, err := readStable(key, MaxSessionBytes)
		exists := err == nil
		if err != nil && !os.IsNotExist(err) {
			return nil, result, err
		}
		// A local-path handoff can use the very same global agent home. Never
		// rewrite the original history to point at a second workspace.
		originalPath := filepath.Join(home, filepath.FromSlash(session.Path))
		if manifest.SourceRoot != root {
			if err := noSymlinks(home, session.Path); err != nil {
				return nil, result, err
			}
			if originalLocal, readErr := readStable(originalPath, MaxSessionBytes); readErr == nil && bytes.HasPrefix(originalLocal, original) {
				result.Skipped++
				result.Warnings = append(result.Warnings, fmt.Sprintf("Skipped %s session %s: source history is already in the same agent home; its workspace path was preserved.", session.Agent, session.ID))
				continue
			}
		}
		if exists {
			if bytes.Equal(previous, incoming) {
				result.Skipped++
			} else if bytes.HasPrefix(incoming, previous) {
				result.Updated++
				plans = append(plans, writePlan{home, relative, incoming, previous, true})
			} else if bytes.HasPrefix(previous, incoming) {
				result.Skipped++
				result.Warnings = append(result.Warnings, fmt.Sprintf("Skipped %s session %s: destination has newer history.", session.Agent, session.ID))
			} else {
				result.Conflicts++
				switch options.ConflictPolicy {
				case "skip":
					result.Skipped++
					result.Warnings = append(result.Warnings, fmt.Sprintf("Skipped divergent %s session %s; destination history was preserved.", session.Agent, session.ID))
				case "theirs":
					result.Updated++
					plans = append(plans, writePlan{home, relative, incoming, previous, true})
					result.Warnings = append(result.Warnings, fmt.Sprintf("Replaced divergent %s session %s with incoming history (--session-conflict=theirs).", session.Agent, session.ID))
				case "keep-both":
					archive, archivePath, archiveErr := conflictPlan(options.ConflictsDir, session, incoming)
					if archiveErr != nil {
						return nil, result, archiveErr
					}
					if archive != nil && !archives[archivePath] {
						plans = append(plans, *archive)
					}
					result.Skipped++
					if !archives[archivePath] {
						result.PreservedPaths = append(result.PreservedPaths, archivePath)
					}
					archives[archivePath] = true
					result.Warnings = append(result.Warnings, fmt.Sprintf("Preserved divergent incoming %s session %s at %s; destination history was preserved.", session.Agent, session.ID, archivePath))
				}
			}
		} else {
			result.Imported++
			plans = append(plans, writePlan{home, relative, incoming, nil, false})
		}
		session.CWD = remap(aliasPath(session.CWD, manifest.SourceAliases), manifest.SourceRoot, root)
		session.Path = relative
		session.SHA256 = digest(incoming)
		session.SizeBytes = int64(len(incoming))
		result.Sessions = append(result.Sessions, session)
	}
	return plans, result, nil
}

// Import installs native logs without replacing unrelated sessions. Identical
// histories are skipped and append-only incoming extensions are accepted. A
// newer destination is skipped; divergence is archived unless options override it.
func Import(bundleDir, projectRoot string, roots Roots) (ImportResult, error) {
	return ImportWithOptions(bundleDir, projectRoot, roots, ImportOptions{})
}

func ImportWithOptions(bundleDir, projectRoot string, roots Roots, options ImportOptions) (ImportResult, error) {
	manifest, err := readManifest(bundleDir)
	if err != nil {
		return ImportResult{}, err
	}
	// Lock only included adapters in a stable order so simultaneous imports cannot
	// race. Native agents do not honor this lock and must be stopped by the user.
	var homes []string
	for _, session := range manifest.Sessions {
		home := roots.CodexHome
		if session.Agent == "claude" {
			home = roots.ClaudeHome
		}
		if home == "" {
			return ImportResult{}, otgerror.New(otgerror.CodeInput, fmt.Sprintf("%s home is required to import captured sessions", session.Agent))
		}
		if err := noSymlinks(home, "."); err != nil {
			return ImportResult{}, err
		}
		abs, err := fsx.Canonical(home)
		if err != nil {
			return ImportResult{}, err
		}
		homes = append(homes, abs)
	}
	sort.Strings(homes)
	var locks []func()
	defer func() {
		for _, lock := range locks {
			lock()
		}
	}()
	for i, home := range homes {
		if i > 0 && home == homes[i-1] {
			continue
		}
		if err := secureMkdir(home, "."); err != nil {
			return ImportResult{}, err
		}
		path := filepath.Join(home, ".onthego-import.lock")
		unlock, err := fsx.Lock(path)
		if err != nil {
			return ImportResult{}, err
		}
		locks = append(locks, unlock)
	}
	plans, result, err := planImportManifest(bundleDir, projectRoot, roots, options, manifest)
	if err != nil {
		return ImportResult{}, err
	}
	for _, plan := range plans {
		if err := secureMkdir(plan.home, filepath.Dir(filepath.FromSlash(plan.relative))); err != nil {
			return ImportResult{}, err
		}
	}
	// Stage every write first. Collision/source checks are repeated immediately
	// before commit, minimizing partial changes on I/O failures.
	staged := make([]string, 0, len(plans))
	defer func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
	}()
	for _, plan := range plans {
		target := filepath.Join(plan.home, filepath.FromSlash(plan.relative))
		file, err := os.CreateTemp(filepath.Dir(target), ".onthego-session-*")
		if err != nil {
			return ImportResult{}, err
		}
		staged = append(staged, file.Name())
		_, writeErr := file.Write(plan.incoming)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil {
			return ImportResult{}, writeErr
		}
		if closeErr != nil {
			return ImportResult{}, closeErr
		}
	}
	for _, plan := range plans {
		if err := noSymlinks(plan.home, plan.relative); err != nil {
			return ImportResult{}, err
		}
		current, err := readStable(filepath.Join(plan.home, filepath.FromSlash(plan.relative)), MaxSessionBytes)
		if plan.exists && (err != nil || !bytes.Equal(current, plan.previous)) || !plan.exists && !os.IsNotExist(err) {
			return ImportResult{}, errors.New("native session changed during import; stop the agent before retrying")
		}
	}
	for i, plan := range plans {
		target := filepath.Join(plan.home, filepath.FromSlash(plan.relative))
		var commitErr error
		if plan.exists {
			commitErr = os.Rename(staged[i], target)
		} else {
			// Hard-linking avoids replacing a file created after preflight.
			commitErr = os.Link(staged[i], target)
		}
		if commitErr != nil {
			return ImportResult{}, errors.Join(commitErr, rollback(plans[:i]))
		}
	}
	return result, nil
}

func readManifest(bundleDir string) (Manifest, error) {
	if err := noSymlinks(bundleDir, "manifest.json"); err != nil {
		return Manifest{}, err
	}
	data, err := readStable(filepath.Join(bundleDir, "manifest.json"), 8<<20)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.SchemaVersion != SchemaVersion || !filepath.IsAbs(manifest.SourceRoot) || filepath.Clean(manifest.SourceRoot) != manifest.SourceRoot || manifest.SourceRoot == string(filepath.Separator) {
		return Manifest{}, otgerror.New(otgerror.CodeSessionFormat, "invalid native session manifest")
	}
	if err := validateAliases(manifest.SourceAliases, manifest.SourceRoot); err != nil {
		return Manifest{}, err
	}
	for _, session := range manifest.Sessions {
		if err := validatePath(session.Agent, session.Path); err != nil {
			return Manifest{}, err
		}
	}
	return manifest, nil
}

func rollback(plans []writePlan) error {
	var failures []error
	for i := len(plans) - 1; i >= 0; i-- {
		plan := plans[i]
		target := filepath.Join(plan.home, filepath.FromSlash(plan.relative))
		current, err := readStable(target, MaxSessionBytes)
		if err != nil || !bytes.Equal(current, plan.incoming) {
			failures = append(failures, fmt.Errorf("cannot roll back session changed by another process: %s", target))
			continue
		}
		if !plan.exists {
			if err := os.Remove(target); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		file, err := os.CreateTemp(filepath.Dir(target), ".onthego-rollback-*")
		if err != nil {
			failures = append(failures, err)
			continue
		}
		_, writeErr := file.Write(plan.previous)
		closeErr := file.Close()
		if writeErr == nil && closeErr == nil {
			writeErr = os.Rename(file.Name(), target)
		}
		_ = os.Remove(file.Name())
		if err := errors.Join(writeErr, closeErr); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// ProjectDirectory matches the project encoding verified against Claude Code
// 2.1.301. JavaScript measures strings in UTF-16 code units, including when
// sanitizing non-BMP characters and hashing long project paths.
func ProjectDirectory(root string) string {
	units := utf16.Encode([]rune(filepath.Clean(root)))
	sanitized := make([]byte, len(units))
	var hash int32
	for i, unit := range units {
		if unit >= 'a' && unit <= 'z' || unit >= 'A' && unit <= 'Z' || unit >= '0' && unit <= '9' {
			sanitized[i] = byte(unit)
		} else {
			sanitized[i] = '-'
		}
		hash = hash*31 + int32(unit)
	}
	if len(sanitized) <= 200 {
		return string(sanitized)
	}
	positive := int64(hash)
	if positive < 0 {
		positive = -positive
	}
	return string(sanitized[:200]) + "-" + strconv.FormatInt(positive, 36)
}

// Read only enough metadata to scope the log before loading its full history.
// An unrelated huge/active history must not block the selected project.
func firstMetadata(path, agent string) (id, cwd string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 4<<20))
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		id, cwd, err = metadata(scanner.Bytes(), agent)
		if err == nil {
			return id, cwd, nil
		}
	}
	// Missing metadata denotes an unsupported/unfinished log, not a session
	// authorized for the requested project. Full selected logs are validated.
	return "", "", nil
}

func metadata(data []byte, agent string) (id, cwd string, err error) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var value map[string]json.RawMessage
		if decodeErr := json.Unmarshal(line, &value); decodeErr != nil {
			return id, cwd, fmt.Errorf("incomplete or malformed native JSONL: %w", decodeErr)
		}
		if agent == "codex" {
			var kind string
			_ = json.Unmarshal(value["type"], &kind)
			if kind != "session_meta" {
				continue
			}
			var payload struct {
				ID        string `json:"id"`
				SessionID string `json:"session_id"`
				CWD       string `json:"cwd"`
			}
			if err := json.Unmarshal(value["payload"], &payload); err != nil {
				return id, cwd, err
			}
			if id == "" {
				id, cwd = payload.ID, payload.CWD
				if id == "" {
					id = payload.SessionID
				}
			}
		} else if id == "" {
			var candidateID, candidateCWD string
			_ = json.Unmarshal(value["sessionId"], &candidateID)
			_ = json.Unmarshal(value["cwd"], &candidateCWD)
			if candidateID != "" && candidateCWD != "" {
				id, cwd = candidateID, candidateCWD
			}
		}
	}
	if id == "" || cwd == "" || !filepath.IsAbs(cwd) {
		return id, cwd, errors.New("native log has no supported session metadata")
	}
	return id, cwd, nil
}

func remap(value, source, target string) string {
	if filepath.IsAbs(value) && value != source && !strings.HasPrefix(value, source+string(filepath.Separator)) {
		if canonical, err := fsx.Canonical(value); err == nil && (canonical == source || strings.HasPrefix(canonical, source+string(filepath.Separator))) {
			value = canonical
		}
	}
	if value == source {
		return target
	}
	if strings.HasPrefix(value, source+string(filepath.Separator)) {
		return target + strings.TrimPrefix(value, source)
	}
	return value
}

func within(path, root string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	canonical, err := fsx.Canonical(path)
	if err != nil || canonical == path {
		return false
	}
	rel, err = filepath.Rel(root, canonical)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func absolute(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func validatePath(agent, path string) error {
	if path == "" || strings.Contains(path, "\\") || strings.ContainsRune(path, 0) || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || strings.HasPrefix(path, "../") {
		return errors.New("unsafe native session path")
	}
	parts := strings.Split(path, "/")
	if filepath.Ext(path) != ".jsonl" {
		return errors.New("native session path must end in .jsonl")
	}
	switch agent {
	case "codex":
		if len(parts) < 2 || (parts[0] != "sessions" && parts[0] != "archived_sessions") {
			return errors.New("invalid Codex session path")
		}
	case "claude":
		if len(parts) < 3 || parts[0] != "projects" {
			return errors.New("invalid Claude session path")
		}
	default:
		return errors.New("unsupported native session agent")
	}
	return nil
}

func noSymlinks(root, relative string) error {
	root, err := absolute(root)
	if err != nil {
		return err
	}
	// The caller-selected root establishes the boundary. Its ancestors may be
	// OS symlinks (for example /var on macOS), but neither root nor its children
	// may redirect imported files outside that boundary.
	current := filepath.Dir(root)
	for _, part := range append([]string{filepath.Base(root)}, strings.Split(filepath.FromSlash(relative), string(filepath.Separator))...) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("native session path contains a symlink: %s", current)
		}
	}
	return nil
}

func secureMkdir(home, relative string) error {
	if err := noSymlinks(home, relative); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(home, relative), 0o700)
}

func readStable(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("native session is not a regular file: %s", path)
	}
	if before.Size() > limit {
		return nil, otgerror.New(otgerror.CodeSessionFormat, fmt.Sprintf("native session file exceeds %d byte limit: %s", limit, path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, otgerror.New(otgerror.CodeSourceChanged, "native session changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || before.Size() != int64(len(data)) || !before.ModTime().Equal(after.ModTime()) || (changeTime(before) != nil && !unchanged(before, after)) {
		return nil, otgerror.New(otgerror.CodeSourceChanged, "native session changed while reading; stop the agent before handoff")
	}
	return data, nil
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
