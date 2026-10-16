package agentsessions

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/otgerror"
)

const DefaultRecentDays = 7

// Selection limits the native histories disclosed in a handoff. Explicit IDs
// override the age window. All and IDs are mutually exclusive.
type Selection struct {
	IDs        []string `json:"ids,omitempty"`
	All        bool     `json:"all,omitempty"`
	None       bool     `json:"none,omitempty"`
	RecentDays int      `json:"recent_days,omitempty"`
}

// ValidateSelection is shared by command previews and actual exports.
func ValidateSelection(selection Selection) error {
	if selection.RecentDays < 0 {
		return otgerror.New(otgerror.CodeInput, "session recent days must not be negative")
	}
	if selection.All && len(selection.IDs) > 0 {
		return otgerror.New(otgerror.CodeInput, "all sessions and explicit session IDs are mutually exclusive")
	}
	if selection.None && (selection.All || len(selection.IDs) > 0) {
		return otgerror.New(otgerror.CodeInput, "no sessions cannot be combined with all sessions or explicit session IDs")
	}
	for _, id := range selection.IDs {
		if strings.TrimSpace(id) == "" {
			return otgerror.New(otgerror.CodeInput, "session ID must not be blank")
		}
	}
	return nil
}

type DiscoveryResult struct {
	Sessions []Session `json:"sessions"`
	Warnings []string  `json:"warnings,omitempty"`
}

type discovered struct {
	session Session
	data    []byte
}
type cachedMetadata struct {
	info    os.FileInfo
	session Session
	warning string
	invalid bool
}

// The cache deliberately retains metadata only, never plaintext transcripts.
// Device/inode, ctime, mtime and size are checked before reuse, so a replacement
// or an edit that restores mtime cannot serve a stale hash. Unknown platforms
// without ctime support simply do not cache.
var metadataCache = struct {
	sync.Mutex
	entries map[string]cachedMetadata
}{entries: make(map[string]cachedMetadata)}

func changeTime(info os.FileInfo) any {
	if info == nil || info.Sys() == nil {
		return nil
	}
	v := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if v.Kind() != reflect.Struct {
		return nil
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		f := v.FieldByName(name)
		if f.IsValid() && f.CanInterface() {
			return f.Interface()
		}
	}
	return nil
}

func unchanged(a, b os.FileInfo) bool {
	return changeTime(a) != nil && stableMetadata(a, b)
}

func stableMetadata(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() && os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime()) && reflect.DeepEqual(changeTime(a), changeTime(b))
}

func loadMetadata(path, agent string, info os.FileInfo) (cachedMetadata, bool) {
	metadataCache.Lock()
	defer metadataCache.Unlock()
	value, ok := metadataCache.entries[agent+":"+path]
	return value, ok && unchanged(value.info, info)
}

func saveMetadata(path, agent string, value cachedMetadata) {
	metadataCache.Lock()
	defer metadataCache.Unlock()
	if len(metadataCache.entries) >= 4096 {
		clear(metadataCache.entries)
	}
	metadataCache.entries[agent+":"+path] = value
}

func Discover(projectRoot string, roots Roots) ([]Session, error) {
	result, err := DiscoverWithOptions(projectRoot, roots, Selection{})
	return result.Sessions, err
}

func DiscoverWithOptions(projectRoot string, roots Roots, selection Selection) (DiscoveryResult, error) {
	files, warnings, err := discover(projectRoot, roots, selection, false)
	result := DiscoveryResult{Sessions: []Session{}, Warnings: warnings}
	for _, file := range files {
		result.Sessions = append(result.Sessions, file.session)
	}
	return result, err
}

func discover(projectRoot string, roots Roots, selection Selection, includeData bool) ([]discovered, []string, error) {
	if err := ValidateSelection(selection); err != nil {
		return nil, nil, err
	}
	if selection.None {
		return nil, nil, nil
	}
	root, err := fsx.Canonical(projectRoot)
	if err != nil {
		return nil, nil, err
	}
	if root == string(filepath.Separator) {
		return nil, nil, otgerror.New(otgerror.CodeInput, "native session project root must not be the filesystem root")
	}
	days := selection.RecentDays
	if days == 0 {
		days = DefaultRecentDays
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	requested := map[string]bool{}
	for _, id := range selection.IDs {
		requested[id] = false
	}
	var result []discovered
	var warnings []string
	var total int64
	for _, adapter := range []struct {
		agent, home string
		dirs        []string
	}{
		{"codex", roots.CodexHome, []string{"sessions", "archived_sessions"}},
		{"claude", roots.ClaudeHome, []string{"projects"}},
	} {
		if adapter.home == "" {
			continue
		}
		startResult, startWarnings, startTotal := len(result), len(warnings), total
		adapterErr := func() error {
			home, err := absolute(adapter.home)
			if err != nil {
				return err
			}
			for _, dir := range adapter.dirs {
				if err := noSymlinks(home, dir); err != nil {
					return err
				}
				err := filepath.WalkDir(filepath.Join(home, dir), func(path string, entry fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						if os.IsNotExist(walkErr) {
							return nil
						}
						return walkErr
					}
					if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".jsonl" || !entry.Type().IsRegular() {
						return nil
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					if !selection.All && len(requested) == 0 && info.ModTime().Before(cutoff) {
						return nil
					}
					cached, hit := loadMetadata(path, adapter.agent, info)
					if !hit {
						id, cwd, err := firstMetadata(path, adapter.agent)
						if err != nil {
							return err
						}
						canonicalCWD, err := fsx.Canonical(cwd)
						if cwd == "" || err != nil || !within(canonicalCWD, root) {
							return nil
						}
						if len(requested) > 0 {
							if _, ok := requested[id]; !ok {
								return nil
							}
						}
						data, err := readStable(path, MaxSessionBytes)
						if err != nil {
							return err
						}
						clean, trimmed, err := completeLines(data)
						warning := ""
						if trimmed {
							warning = fmt.Sprintf("Ignored incomplete final JSONL line in %s session %s (%s).", adapter.agent, id, path)
						}
						if err != nil {
							warning = fmt.Sprintf("Skipped malformed %s session %s (%s): %v", adapter.agent, id, path, err)
						} else {
							var validatedID, validatedCWD string
							validatedID, validatedCWD, err = metadata(clean, adapter.agent)
							if err != nil || id != validatedID || cwd != validatedCWD {
								warning = fmt.Sprintf("Skipped malformed %s session %s (%s): invalid session metadata.", adapter.agent, id, path)
								err = errors.New("invalid metadata")
							}
						}
						rel, relErr := filepath.Rel(home, path)
						if relErr != nil {
							return relErr
						}
						if relErr := validatePath(adapter.agent, filepath.ToSlash(rel)); relErr != nil {
							return relErr
						}
						after, statErr := os.Lstat(path)
						if statErr != nil {
							return statErr
						}
						if !stableMetadata(info, after) {
							return otgerror.New(otgerror.CodeSourceChanged, "native session changed during discovery; stop the agent before handoff")
						}
						cached = cachedMetadata{info: after, session: Session{Agent: adapter.agent, ID: id, CWD: cwd, Path: filepath.ToSlash(rel), SizeBytes: int64(len(clean)), SHA256: digest(clean), LastActive: after.ModTime().UTC().Format(time.RFC3339Nano)}, warning: warning, invalid: err != nil}
						saveMetadata(path, adapter.agent, cached)
						if includeData && !cached.invalid { // The verified bytes are already available.
							if cached.warning != "" {
								warnings = append(warnings, cached.warning)
							}
							total += int64(len(clean))
							if total > MaxBundleBytes {
								return otgerror.New(otgerror.CodeSessionFormat, "native session bundle exceeds 1 GiB")
							}
							result = append(result, discovered{cached.session, clean})
							return nil
						}
					}
					canonicalCWD, err := fsx.Canonical(cached.session.CWD)
					if err != nil || !within(canonicalCWD, root) {
						return nil
					}
					if len(requested) > 0 {
						if _, ok := requested[cached.session.ID]; !ok {
							return nil
						}
					}
					if cached.warning != "" {
						warnings = append(warnings, cached.warning)
					}
					if cached.invalid {
						return nil
					}
					var data []byte
					if includeData {
						raw, err := readStable(path, MaxSessionBytes)
						if err != nil {
							return err
						}
						data, _, err = completeLines(raw)
						if err != nil {
							return err
						}
						if digest(data) != cached.session.SHA256 {
							return otgerror.New(otgerror.CodeSourceChanged, "native session changed after discovery; retry handoff")
						}
					}
					total += cached.session.SizeBytes
					if total > MaxBundleBytes {
						return otgerror.New(otgerror.CodeSessionFormat, "native session bundle exceeds 1 GiB")
					}
					result = append(result, discovered{cached.session, data})
					return nil
				})
				if err != nil {
					return err
				}
			}
			return nil
		}()
		if adapterErr != nil {
			var typed *otgerror.Error
			if errors.As(adapterErr, &typed) {
				return nil, warnings, adapterErr
			}
			// A failed adapter contributes no histories, even if an earlier
			// directory was readable. The other adapter remains independent.
			result, warnings, total = result[:startResult], warnings[:startWarnings], startTotal
			warnings = append(warnings, fmt.Sprintf("Skipped %s sessions from %s: %v", adapter.agent, adapter.home, adapterErr))
		}
	}
	for _, file := range result {
		requestedMark(requested, file.session.ID)
	}
	var missing []string
	for id, found := range requested {
		if !found {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		sort.Strings(warnings)
		message := fmt.Sprintf("Requested sessions were not found as valid histories for this project: %s", strings.Join(missing, ", "))
		if len(warnings) > 0 {
			message += ". Discovery warnings: " + strings.Join(warnings, "; ")
		}
		return nil, warnings, otgerror.New(otgerror.CodeInput, message)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].session.Agent+result[i].session.Path < result[j].session.Agent+result[j].session.Path
	})
	sort.Strings(warnings)
	return result, warnings, nil
}

func requestedMark(requested map[string]bool, id string) {
	if _, ok := requested[id]; ok {
		requested[id] = true
	}
}

// Only a malformed suffix after the last newline is discarded. A completed
// malformed line excludes the session, including a malformed final line that
// ended with a newline. Valid final lines without newlines remain byte exact.
func completeLines(data []byte) ([]byte, bool, error) {
	offset := 0
	for offset < len(data) {
		next := bytes.IndexByte(data[offset:], '\n')
		end := len(data)
		if next >= 0 {
			end = offset + next
		}
		line := data[offset:end]
		if len(bytes.TrimSpace(line)) > 0 && !jsonValidObject(line) {
			if next < 0 {
				return data[:offset], true, nil
			}
			return nil, false, fmt.Errorf("malformed JSONL at byte %d", offset)
		}
		if next < 0 {
			break
		}
		offset = end + 1
	}
	return data, false, nil
}
