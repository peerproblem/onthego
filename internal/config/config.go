package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
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
	data, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if cfg.SchemaVersion != 1 && cfg.SchemaVersion != SchemaVersion {
		return Config{}, fmt.Errorf("unsupported or incomplete %s", FileName)
	}
	if err := ValidateProjectID(cfg.ProjectID); err != nil {
		return Config{}, err
	}
	// Old cloud settings are intentionally discarded. Personal devices belong in
	// the user's private config, never in a shared project file.
	cfg.SchemaVersion = SchemaVersion
	if cfg.Agent == "" {
		cfg.Agent = "auto"
	}
	return cfg, nil
}

func Save(root string, cfg Config) error {
	if err := ValidateProjectID(cfg.ProjectID); err != nil {
		return err
	}
	cfg.SchemaVersion = SchemaVersion
	if cfg.Agent == "" {
		cfg.Agent = "auto"
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	path := filepath.Join(root, FileName)
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
	if override := os.Getenv("ONTHEGO_DATA_HOME"); override != "" {
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
