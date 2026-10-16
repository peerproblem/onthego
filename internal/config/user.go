package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

const UserFileName = "config.yaml"

// Device is a peer owned by the user. SSH uses the user's existing SSH agent
// and configuration; onthego does not need or store a service API key.
type Device struct {
	Local         bool   `yaml:"local,omitempty" json:"local,omitempty"`
	Home          string `yaml:"home,omitempty" json:"home,omitempty"`
	Host          string `yaml:"host,omitempty" json:"host,omitempty"`
	Path          string `yaml:"path,omitempty" json:"path,omitempty"`
	LocalPath     string `yaml:"local_path,omitempty" json:"local_path,omitempty"`
	Port          int    `yaml:"port,omitempty" json:"port,omitempty"`
	IdentityFile  string `yaml:"identity_file,omitempty" json:"identity_file,omitempty"`
	Binary        string `yaml:"binary,omitempty" json:"binary,omitempty"`
	AgeRecipient  string `yaml:"age_recipient,omitempty" json:"age_recipient,omitempty"`
	SigningPublic string `yaml:"signing_public,omitempty" json:"signing_public,omitempty"`
}

type UserConfig struct {
	SchemaVersion int                        `yaml:"schema_version" json:"schema_version"`
	Devices       map[string]Device          `yaml:"devices" json:"devices"`
	Trusted       map[string]TrustedIdentity `yaml:"trusted,omitempty" json:"trusted,omitempty"`
}

func UserConfigPath() (string, error) {
	base, err := DataRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, UserFileName), nil
}

func LoadUser() (UserConfig, error) {
	file, err := UserConfigPath()
	if err != nil {
		return UserConfig{}, err
	}
	if err := EnsurePrivateDir(filepath.Dir(file)); err != nil {
		return UserConfig{}, err
	}
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return UserConfig{SchemaVersion: SchemaVersion, Devices: map[string]Device{}}, nil
	}
	if err != nil {
		return UserConfig{}, err
	}
	if !info.Mode().IsRegular() {
		return UserConfig{}, fmt.Errorf("user config must be a regular file: %s", file)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return UserConfig{}, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return UserConfig{}, err
	}
	var cfg UserConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return UserConfig{}, fmt.Errorf("parse user config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return UserConfig{}, errors.New("user config must contain one YAML document")
	}
	if err := validateUser(cfg); err != nil {
		return UserConfig{}, err
	}
	if cfg.Devices == nil {
		cfg.Devices = map[string]Device{}
	}
	return cfg, nil
}

func SaveUser(cfg UserConfig) error {
	cfg.SchemaVersion = SchemaVersion
	if cfg.Devices == nil {
		cfg.Devices = map[string]Device{}
	}
	if err := validateUser(cfg); err != nil {
		return err
	}
	file, err := UserConfigPath()
	if err != nil {
		return err
	}
	if err := EnsurePrivateDir(filepath.Dir(file)); err != nil {
		return err
	}
	if info, err := os.Lstat(file); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("refusing to replace a non-regular user config")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

type TrustedIdentity struct {
	AgeRecipient  string `yaml:"age_recipient" json:"age_recipient"`
	SigningPublic string `yaml:"signing_public" json:"signing_public"`
}

func validateUser(cfg UserConfig) error {
	if cfg.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported user config schema %d (expected %d)", cfg.SchemaVersion, SchemaVersion)
	}
	for name, trusted := range cfg.Trusted {
		if err := ValidateDeviceName(name); err != nil {
			return err
		}
		if err := ValidateDevice(Device{Local: true, AgeRecipient: trusted.AgeRecipient, SigningPublic: trusted.SigningPublic}); err != nil {
			return err
		}
		if trusted.AgeRecipient == "" || trusted.SigningPublic == "" {
			return errors.New("trusted identity requires both public keys")
		}
	}
	for name, device := range cfg.Devices {
		if err := ValidateDeviceName(name); err != nil {
			return err
		}
		if err := ValidateDevice(device); err != nil {
			return fmt.Errorf("device %q: %w", name, err)
		}
	}
	return nil
}

var deviceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var sshUserPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
var sshHostPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]*$|^\[[A-Fa-f0-9:.]+\]$`)
var binaryPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func ValidateDeviceName(name string) error {
	if !deviceNamePattern.MatchString(name) {
		return errors.New("device name must start with a letter or number and contain only letters, numbers, dots, hyphens or underscores")
	}
	return nil
}

func ValidateDevice(device Device) error {
	if device.Home != "" && (!filepath.IsAbs(device.Home) || hasControl(device.Home)) {
		return errors.New("device home must be an absolute path without control characters")
	}
	if (device.Host == "") == (!device.Local && device.LocalPath == "") {
		return errors.New("set exactly one of host or local_path")
	}
	if device.Local || device.LocalPath != "" {
		if device.LocalPath != "" && (!filepath.IsAbs(device.LocalPath) || hasControl(device.LocalPath)) {
			return errors.New("local_path must be an absolute path without control characters")
		}
		if device.Path != "" || device.Port != 0 || device.IdentityFile != "" {
			return errors.New("local devices cannot have SSH path, port or identity_file settings")
		}
	} else {
		if err := ValidateHost(device.Host); err != nil {
			return err
		}
		if device.Path != "" && (!path.IsAbs(device.Path) || hasControl(device.Path)) {
			return errors.New("path must be an absolute remote project path without control characters")
		}
		if device.Port < 0 || device.Port > 65535 {
			return errors.New("port must be between 1 and 65535, or omitted")
		}
		if device.IdentityFile != "" && (!filepath.IsAbs(device.IdentityFile) || hasControl(device.IdentityFile)) {
			return errors.New("identity_file must be an absolute path without control characters")
		}
	}
	// Transports pass absolute paths as a single argument (and quote them for
	// SSH), so installation directories may contain spaces or punctuation.
	if device.Binary != "" && (hasControl(device.Binary) || !binaryPattern.MatchString(device.Binary) && !path.IsAbs(device.Binary)) {
		return errors.New("binary must be an executable name or absolute path without control characters")
	}
	if device.AgeRecipient != "" {
		if _, err := age.ParseX25519Recipient(device.AgeRecipient); err != nil {
			return fmt.Errorf("invalid age recipient: %w", err)
		}
	}
	if device.SigningPublic != "" {
		key, err := base64.StdEncoding.DecodeString(device.SigningPublic)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return errors.New("invalid signing public key")
		}
	}
	return nil
}

func ValidateHost(host string) error {
	parts := strings.Split(host, "@")
	if len(parts) > 2 || len(parts) == 2 && !sshUserPattern.MatchString(parts[0]) || !sshHostPattern.MatchString(parts[len(parts)-1]) {
		return errors.New("host must be an SSH alias, hostname, or user@host without shell characters or options")
	}
	return nil
}

func hasControl(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// EnsurePrivateDir creates a private directory or tightens an existing one. A
// symlink is rejected so keys and configuration cannot silently move elsewhere.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private directory must not be a symlink or file: %s", dir)
	}
	return os.Chmod(dir, 0o700)
}
