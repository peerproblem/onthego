package agentsessions

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/otgerror"
)

func TestNoSessionsSkipsDiscoveryAndExportDirectory(t *testing.T) {
	base := canonicalTestDir(t)
	project := filepath.Join(base, "project")
	home := filepath.Join(base, "broken-home")
	if err := os.Symlink(filepath.Join(base, "missing"), home); err != nil {
		t.Fatal(err)
	}
	var selection Selection
	if err := json.Unmarshal([]byte(`{"none":true}`), &selection); err != nil {
		t.Fatal(err)
	}
	result, err := DiscoverWithOptions(project, Roots{CodexHome: home}, selection)
	if err != nil || len(result.Sessions) != 0 || len(result.Warnings) != 0 {
		t.Fatalf("disabled discovery: %#v, %v", result, err)
	}
	bundle := filepath.Join(base, "native-sessions")
	manifest, err := ExportWithOptions(project, bundle, Roots{CodexHome: home}, selection)
	if err != nil || len(manifest.Sessions) != 0 || len(manifest.Warnings) != 0 {
		t.Fatalf("disabled export: %#v, %v", manifest, err)
	}
	if _, err := os.Lstat(bundle); !os.IsNotExist(err) {
		t.Fatalf("disabled export created a bundle: %v", err)
	}
}

func TestSelectionErrorsAreInputErrors(t *testing.T) {
	for name, raw := range map[string]string{
		"negative days": `{"recent_days":-1}`,
		"blank ID":      `{"ids":["  "]}`,
		"all and ID":    `{"all":true,"ids":["id"]}`,
		"none and ID":   `{"none":true,"ids":["id"]}`,
		"none and all":  `{"none":true,"all":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			var selection Selection
			if err := json.Unmarshal([]byte(raw), &selection); err != nil {
				t.Fatal(err)
			}
			var validated *otgerror.Error
			if err := ValidateSelection(selection); !errors.As(err, &validated) || validated.Code != otgerror.CodeInput {
				t.Fatalf("shared validator did not return E_INPUT: %v", err)
			}
			_, err := DiscoverWithOptions(t.TempDir(), Roots{}, selection)
			if err == nil || otgerror.Public(err).Code != otgerror.CodeInput {
				t.Fatalf("expected typed input error: %v", err)
			}
			var typed *otgerror.Error
			if !(errors.As(err, &typed) && typed.Code == otgerror.CodeInput) {
				t.Fatalf("error is not typed E_INPUT: %v", err)
			}
		})
	}
}

func TestSelectionNoneOmitsFieldUnlessEnabled(t *testing.T) {
	encoded, err := json.Marshal(Selection{RecentDays: DefaultRecentDays})
	if err != nil || strings.Contains(string(encoded), "none") {
		t.Fatalf("ordinary selection changed state compatibility: %s, %v", encoded, err)
	}
	selection := Selection{None: true, RecentDays: DefaultRecentDays}
	if err := ValidateSelection(selection); err != nil {
		t.Fatalf("default age window conflicts with no sessions: %v", err)
	}
	encoded, err = json.Marshal(selection)
	if err != nil || !strings.Contains(string(encoded), `"none":true`) {
		t.Fatalf("no sessions selection was not persisted: %s, %v", encoded, err)
	}
}

func TestExplicitMissingSessionsFailBeforeCreatingBundle(t *testing.T) {
	base := canonicalTestDir(t)
	project := filepath.Join(base, "project")
	roots := Roots{CodexHome: filepath.Join(base, "codex")}
	writeTest(t, filepath.Join(roots.CodexHome, "sessions", "present.jsonl"), codexLog("present", project))
	bundle := filepath.Join(base, "native-sessions")
	_, err := ExportWithOptions(project, bundle, roots, Selection{IDs: []string{"missing-b", "present", "missing-a"}})
	var typed *otgerror.Error
	if !(errors.As(err, &typed) && typed.Code == otgerror.CodeInput) || !strings.Contains(typed.Message, "missing-a") || !strings.Contains(typed.Message, "missing-b") {
		t.Fatalf("missing session error: %v", err)
	}
	if _, err := os.Lstat(bundle); !os.IsNotExist(err) {
		t.Fatalf("failed export created a bundle: %v", err)
	}
}

func TestDiscoverySkipsOnlyBrokenAdapter(t *testing.T) {
	for _, failure := range []string{"home-symlink", "directory-symlink", "walk-error"} {
		t.Run(failure, func(t *testing.T) {
			base := canonicalTestDir(t)
			project := filepath.Join(base, "project")
			roots := Roots{CodexHome: filepath.Join(base, "codex"), ClaudeHome: filepath.Join(base, "claude")}
			writeTest(t, filepath.Join(roots.ClaudeHome, "projects", ProjectDirectory(project), "healthy.jsonl"), claudeLog("healthy", project))
			switch failure {
			case "home-symlink":
				if err := os.Symlink(roots.ClaudeHome, roots.CodexHome); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				writeTest(t, filepath.Join(roots.CodexHome, "sessions", "partial.jsonl"), codexLog("partial", project))
				if err := os.Symlink(roots.ClaudeHome, filepath.Join(roots.CodexHome, "archived_sessions")); err != nil {
					t.Fatal(err)
				}
			case "walk-error":
				writeTest(t, roots.CodexHome, "regular file instead of home")
			}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := DiscoverWithOptions(project, roots, Selection{})
				if err != nil || len(result.Sessions) != 1 || result.Sessions[0].ID != "healthy" || len(result.Warnings) == 0 || !strings.Contains(strings.Join(result.Warnings, " "), "codex") {
					t.Fatalf("adapter-isolated discovery: %#v, %v", result, err)
				}
			}
			manifest, err := ExportWithOptions(project, filepath.Join(base, "native-sessions"), roots, Selection{})
			if err != nil || len(manifest.Sessions) != 1 || manifest.Sessions[0].ID != "healthy" {
				t.Fatalf("adapter-isolated export: %#v, %v", manifest, err)
			}
			selected, err := DiscoverWithOptions(project, roots, Selection{IDs: []string{"healthy"}})
			if err != nil || len(selected.Sessions) != 1 || len(selected.Warnings) == 0 {
				t.Fatalf("unrelated adapter blocked explicit selection: %#v, %v", selected, err)
			}
			_, err = DiscoverWithOptions(project, roots, Selection{IDs: []string{"missing"}})
			var typed *otgerror.Error
			if !(errors.As(err, &typed) && typed.Code == otgerror.CodeInput) || !strings.Contains(err.Error(), "codex") {
				t.Fatalf("missing requested session must include adapter cause: %v", err)
			}
		})
	}
}

func TestImportOnlyTouchesIncludedAgentHomes(t *testing.T) {
	base := canonicalTestDir(t)
	source, target := filepath.Join(base, "source"), filepath.Join(base, "target")
	sourceHome := filepath.Join(base, "source-codex")
	writeTest(t, filepath.Join(sourceHome, "sessions", "session.jsonl"), codexLog("session", source))
	bundle := filepath.Join(base, "native-sessions")
	if _, err := Export(source, bundle, Roots{CodexHome: sourceHome}); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(base, "broken-claude")
	if err := os.Symlink(filepath.Join(base, "missing"), broken); err != nil {
		t.Fatal(err)
	}
	result, err := Import(bundle, target, Roots{CodexHome: filepath.Join(base, "dest-codex"), ClaudeHome: broken})
	if err != nil || result.Imported != 1 {
		t.Fatalf("unused home blocked import: %#v, %v", result, err)
	}
	emptyBundle := filepath.Join(base, "empty-sessions")
	if _, err := Export(source, emptyBundle, Roots{}); err != nil {
		t.Fatal(err)
	}
	untouched := filepath.Join(base, "untouched-home")
	if _, err := Import(emptyBundle, target, Roots{CodexHome: untouched, ClaudeHome: broken}); err != nil {
		t.Fatalf("empty import: %v", err)
	}
	if _, err := os.Lstat(untouched); !os.IsNotExist(err) {
		t.Fatalf("empty import created an agent home: %v", err)
	}
}

func TestInvalidManifestDoesNotCreateAgentHomes(t *testing.T) {
	base := canonicalTestDir(t)
	bundle := filepath.Join(base, "native-sessions")
	writeTest(t, filepath.Join(bundle, "manifest.json"), "invalid JSON")
	home := filepath.Join(base, "codex")
	if _, err := Import(bundle, filepath.Join(base, "project"), Roots{CodexHome: home}); err == nil {
		t.Fatal("invalid manifest accepted")
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("invalid manifest created home: %v", err)
	}
}
