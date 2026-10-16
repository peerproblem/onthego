package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

const SchemaVersion = 3

type File struct {
	RelativePath string `json:"relative_path"`
	ArchivePath  string `json:"archive_path,omitempty"`
	Kind         string `json:"kind"`
	SizeBytes    int64  `json:"size_bytes"`
	Mode         uint32 `json:"mode"`
	SHA256       string `json:"sha256"`
	Secret       bool   `json:"secret"`
	PolicySource string `json:"policy_source"`
	LinkTarget   string `json:"link_target,omitempty"`
}

type Git struct {
	OriginURL     string `json:"origin_url,omitempty"`
	BundleSHA256  string `json:"bundle_sha256"`
	WIPCommit     string `json:"wip_commit"`
	IndexTree     string `json:"index_tree"`
	WorktreeTree  string `json:"worktree_tree"`
	UntrackedTree string `json:"untracked_tree"`
	Ref           string `json:"ref"`
}

type Agent struct {
	Kind          string `json:"kind"`
	Version       string `json:"version,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	ContextSHA256 string `json:"context_sha256,omitempty"`
}

type Toolchain struct {
	GitVersion string `json:"git_version"`
	Onthego    string `json:"onthego_version"`
}

type Manifest struct {
	SchemaVersion     int               `json:"schema_version"`
	SnapshotID        string            `json:"snapshot_id"`
	ProjectID         string            `json:"project_id"`
	ParentSnapshotIDs []string          `json:"parent_snapshot_ids,omitempty"`
	SourceGitHead     string            `json:"source_git_head"`
	SourceBranch      string            `json:"source_branch"`
	CreatedAt         time.Time         `json:"created_at"`
	Files             []File            `json:"files,omitempty"`
	Git               Git               `json:"git"`
	Agent             Agent             `json:"agent"`
	Toolchain         Toolchain         `json:"toolchain"`
	PayloadHashes     map[string]string `json:"payload_hashes"`
	PayloadSizes      map[string]int64  `json:"payload_sizes,omitempty"`
	RetainedPaths     []string          `json:"retained_paths,omitempty"`
	SignerPublic      string            `json:"signer_public"`
}

func (m Manifest) Canonical() ([]byte, error) {
	copy := m
	copy.SnapshotID = ""
	copy.ParentSnapshotIDs = append([]string(nil), m.ParentSnapshotIDs...)
	sort.Strings(copy.ParentSnapshotIDs)
	copy.RetainedPaths = append([]string(nil), m.RetainedPaths...)
	sort.Strings(copy.RetainedPaths)
	copy.Files = append([]File(nil), m.Files...)
	sort.Slice(copy.Files, func(i, j int) bool { return copy.Files[i].RelativePath < copy.Files[j].RelativePath })
	return json.Marshal(copy)
}

func (m *Manifest) SetSnapshotID() error {
	canonical, err := m.Canonical()
	if err != nil {
		return err
	}
	hash := sha256.Sum256(canonical)
	m.SnapshotID = hex.EncodeToString(hash[:])
	return nil
}

func (m Manifest) ValidateID() error {
	copy := m
	if err := copy.SetSnapshotID(); err != nil {
		return err
	}
	if copy.SnapshotID != m.SnapshotID {
		return errors.New("snapshot ID does not match canonical manifest")
	}
	return nil
}
