package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/peerproblem/onthego/internal/otgerror"
	"gopkg.in/yaml.v3"
)

const FileName = ".onthego.yaml"

const SchemaVersion = 2

var gitignoreEntries = []string{
	".env",
	".env.*",
	"*.env",
	".onthego.local/",
	"*.otg",
}

type Config struct {
	SchemaVersion int      `yaml:"schema_version" json:"schema_version"`
	ProjectID     string   `yaml:"project_id" json:"project_id"`
	Include       []string `yaml:"include,omitempty" json:"include,omitempty"`
	Exclude       []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
	Agent         string   `yaml:"agent" json:"agent"`
}

func Default() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		ProjectID:     uuid.NewString(),
		Include:       []string{FileName},
		Exclude: []string{
			".git", ".onthego.local", "node_modules", "vendor", "dist", "build", ".cache", "__pycache__",
		},
		Agent: "auto",
	}
}

func FindRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", errors.New("not inside a Git repository")
		}
		abs = parent
	}
}

func Load(root string) (Config, error) {
	path := filepath.Join(root, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, policyError(path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a project policy, migrating legacy cloud policies
// without retaining their obsolete fields. Current policies reject unknown keys.
func Parse(data []byte) (Config, error) {
	var header struct {
		SchemaVersion int `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(data, &header); err != nil {
		return Config{}, otgerror.New(otgerror.CodeInput, "invalid project policy YAML: "+err.Error())
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(header.SchemaVersion == SchemaVersion)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, otgerror.New(otgerror.CodeInput, "invalid project policy YAML (allowed keys: schema_version, project_id, include, exclude, agent): "+err.Error())
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, otgerror.New(otgerror.CodeInput, "project policy must contain one YAML document")
	}
	if cfg.SchemaVersion == 1 && cfg.Agent == "" {
		cfg.Agent = "auto"
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	// Old cloud settings are intentionally discarded. Personal devices belong in
	// the user's private config, never in a shared project file.
	cfg.SchemaVersion = SchemaVersion
	return cfg, nil
}

// Validate checks policy values before they can affect workspace or session
// selection. Only Parse and Save normalize a missing legacy agent to auto.
func Validate(cfg Config) error {
	if cfg.SchemaVersion != 1 && cfg.SchemaVersion != SchemaVersion {
		return otgerror.New(otgerror.CodeInput, fmt.Sprintf("invalid schema_version %d; allowed values: 1, %d", cfg.SchemaVersion, SchemaVersion))
	}
	if err := ValidateProjectID(cfg.ProjectID); err != nil {
		return otgerror.New(otgerror.CodeInput, fmt.Sprintf("invalid project_id %q; allowed: 1-128 letters, numbers, dots, hyphens or underscores, except . and ..", cfg.ProjectID))
	}
	switch cfg.Agent {
	case "auto", "codex", "claude", "none":
		return nil
	default:
		return otgerror.New(otgerror.CodeInput, fmt.Sprintf("invalid agent %q; allowed values: auto, codex, claude, none", cfg.Agent))
	}
}

func policyError(path string, err error) error {
	return otgerror.Wrap(otgerror.CodeInput, fmt.Sprintf("invalid project policy %s: %s", path, otgerror.Public(err).Message), err)
}

func Save(root string, cfg Config) error {
	path := filepath.Join(root, FileName)
	if cfg.SchemaVersion == 1 && cfg.Agent == "" {
		cfg.Agent = "auto"
	}
	if err := Validate(cfg); err != nil {
		return policyError(path, err)
	}
	cfg.SchemaVersion = SchemaVersion
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".onthego.yaml-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func EnsureGitignore(root string) (bool, error) {
	path := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("refusing to modify a symlinked .gitignore")
	}
	existing := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		existing[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, entry := range gitignoreEntries {
		if !existing[entry] {
			missing = append(missing, entry)
		}
	}
	if len(missing) == 0 {
		return false, nil
	}
	updated := append([]byte(nil), data...)
	if len(updated) > 0 && updated[len(updated)-1] != '\n' {
		updated = append(updated, '\n')
	}
	if len(updated) > 0 {
		updated = append(updated, '\n')
	}
	updated = append(updated, []byte("# ONTHEGO local files\n")...)
	updated = append(updated, []byte(strings.Join(missing, "\n")+"\n")...)
	tmp, err := os.CreateTemp(root, ".gitignore-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(updated); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, err
	}
	return true, nil
}

func ValidateRelative(root, raw string) (string, error) {
	if raw == "" || filepath.IsAbs(raw) {
		return "", fmt.Errorf("path must be project-relative: %q", raw)
	}
	clean := filepath.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes project root: %q", raw)
	}
	abs, err := filepath.Abs(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes project root: %q", raw)
	}
	return filepath.ToSlash(rel), nil
}

func DataRoot() (string, error) {
	if override := os.Getenv("ONTHEGO_HOME"); override != "" {
		return filepath.Abs(override)
	}
	root, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ".onthego"), nil
}

func ProjectDataRoot(projectID string) (string, error) {
	base, err := DataRoot()
	if err != nil {
		return "", err
	}
	if err := ValidateProjectID(projectID); err != nil {
		return "", err
	}
	return filepath.Join(base, "projects", projectID), nil
}

func ValidateProjectID(projectID string) error {
	if len(projectID) == 0 || len(projectID) > 128 || projectID == "." || projectID == ".." {
		return errors.New("invalid project ID")
	}
	for _, ch := range projectID {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return errors.New("invalid project ID: use letters, numbers, dots, hyphens or underscores")
		}
	}
	return nil
}
