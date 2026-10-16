package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProjectPeers separates project locations from a device's pinned identity.
type ProjectPeers struct {
	Peers map[string]string `yaml:"peers" json:"peers"`
}

func ProjectPeersPath(projectID string) (string, error) {
	root, err := ProjectDataRoot(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "peers.yaml"), nil
}
func LoadProjectPeers(projectID string) (ProjectPeers, error) {
	file, err := ProjectPeersPath(projectID)
	if err != nil {
		return ProjectPeers{}, err
	}
	cfg := ProjectPeers{Peers: map[string]string{}}
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if !info.Mode().IsRegular() {
		return cfg, errors.New("project peers must be a regular file")
	}
	if err = os.Chmod(file, 0o600); err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err = dec.Decode(&cfg); err != nil {
		return cfg, err
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return cfg, errors.New("project peers must contain one YAML document")
	}
	if cfg.Peers == nil {
		cfg.Peers = map[string]string{}
	}
	for name, root := range cfg.Peers {
		if err = validatePeerPath(name, root); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}
func SetProjectPeer(projectID, name, root string) error {
	if err := validatePeerPath(name, root); err != nil {
		return err
	}
	cfg, err := LoadProjectPeers(projectID)
	if err != nil {
		return err
	}
	cfg.Peers[name] = filepath.Clean(root)
	file, err := ProjectPeersPath(projectID)
	if err != nil {
		return err
	}
	if err = EnsurePrivateDir(filepath.Dir(file)); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".peers-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), file)
}
func validatePeerPath(name, root string) error {
	if err := ValidateDeviceName(name); err != nil {
		return err
	}
	if !filepath.IsAbs(root) || hasControl(root) {
		return fmt.Errorf("peer %s path must be absolute without control characters", name)
	}
	return nil
}
