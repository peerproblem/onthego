package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/packageotg"
	"github.com/peerproblem/onthego/internal/snapshot"
)

const DefaultBackupLimit = 5

type Result struct {
	Manifest snapshot.Manifest `json:"manifest"`
	Root     string            `json:"root"`
	Backup   string            `json:"backup,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
}

type Backup struct {
	Ref       string    `json:"ref"`
	Commit    string    `json:"commit"`
	CreatedAt time.Time `json:"created_at"`
	Head      string    `json:"head"`
	Branch    string    `json:"branch"`
}

type backupState struct {
	WIP           gitx.WIP          `json:"wip"`
	Files         []snapshot.File   `json:"files,omitempty"`
	AfterPaths    []string          `json:"after_paths"`
	CreatedAt     time.Time         `json:"created_at"`
	SecretPaths   []string          `json:"secret_paths,omitempty"`
	SecretArchive string            `json:"secret_archive,omitempty"`
	SecretSHA256  string            `json:"secret_sha256,omitempty"`
	BranchRefs    map[string]string `json:"branch_refs,omitempty"`
}

// Package restores a standalone package into a new directory. Sessions remain
// only in the private extraction directory and are removed with it.
func Package(ctx context.Context, path, output string, secret age.Identity, signer string) (Result, error) {
	if _, e := os.Lstat(output); e == nil {
		return Result{}, otgerror.New(otgerror.CodeInput, "restore output already exists")
	} else if !os.IsNotExist(e) {
		return Result{}, e
	}
	temp, e := tempDir("restore-")
	if e != nil {
		return Result{}, e
	}
	defer fsx.ReleaseTemp(temp)
	extracted := filepath.Join(temp, "payload")
	m, e := packageotg.ExtractContext(ctx, path, extracted, secret, signer)
	if e != nil {
		return Result{}, e
	}
	// Authenticate and validate objects before publishing a destination directory.
	if _, e = prepare(ctx, extracted, m, ""); e != nil {
		return Result{}, e
	}
	return Apply(ctx, extracted, output, m, nil)
}

// Config imports incoming objects without touching working files, then returns
// the authenticated project policy. Missing destinations are never created.
func Config(ctx context.Context, extracted string, m snapshot.Manifest, existingRoot string) (config.Config, error) {
	root, cleanup, e := objectRoot(ctx, existingRoot, m.SourceGitHead)
	if e != nil {
		return config.Config{}, e
	}
	defer cleanup()
	if e = importBundle(ctx, root, extracted, m); e != nil {
		return config.Config{}, e
	}
	if e = validateState(ctx, root, m); e != nil {
		return config.Config{}, e
	}
	var data []byte
	for _, tree := range []string{m.Git.UntrackedTree, m.Git.WorktreeTree} {
		r, err := gitx.Run(ctx, root, "show", tree+":"+config.FileName)
		if err == nil {
			data = r.Stdout
			break
		}
	}
	if len(data) == 0 {
		return config.Config{}, incomingPolicyError("snapshot has no project configuration", nil)
	}
	cfg, e := config.Parse(data)
	if e != nil {
		return cfg, incomingPolicyError("snapshot project configuration is invalid", e)
	}
	if e = config.Validate(cfg); e != nil {
		return cfg, incomingPolicyError("snapshot project configuration is invalid", e)
	}
	if cfg.ProjectID != m.ProjectID {
		return cfg, incomingPolicyError("snapshot project IDs differ", nil)
	}
	return cfg, nil
}

func prepare(ctx context.Context, extracted string, m snapshot.Manifest, existing string) (string, error) {
	root, cleanup, e := objectRoot(ctx, existing, m.SourceGitHead)
	if e != nil {
		return "", e
	}
	defer cleanup()
	if e = importBundle(ctx, root, extracted, m); e != nil {
		return "", e
	}
	return root, validateState(ctx, root, m)
}

func objectRoot(ctx context.Context, existing, head string) (string, func(), error) {
	if existing != "" {
		if _, e := gitx.Run(ctx, existing, "rev-parse", "--git-dir"); e == nil {
			return existing, func() {}, nil
		}
	}
	temp, e := tempDir("objects-")
	if e != nil {
		return "", nil, e
	}
	root := filepath.Join(temp, "repo")
	if e = os.Mkdir(root, 0700); e != nil {
		fsx.ReleaseTemp(temp)
		return "", nil, e
	}
	if _, e = gitx.Run(ctx, root, "init", "--quiet", objectFormatArgument(head)); e != nil {
		fsx.ReleaseTemp(temp)
		return "", nil, e
	}
	return root, func() { _ = fsx.ReleaseTemp(temp) }, nil
}

func importBundle(ctx context.Context, root, extracted string, m snapshot.Manifest) error {
	if !gitx.ValidOID(m.Git.WIPCommit) {
		return restoreError("invalid WIP commit", nil)
	}
	bundle := filepath.Join(extracted, "repo.bundle")
	if _, e := gitx.Run(ctx, root, "bundle", "verify", bundle); e != nil {
		return restoreError("Git bundle prerequisites are unavailable", e)
	}
	// Fetch all advertised refs into an isolated namespace. In particular, stash
	// and local branch refs at the recipient must not be replaced by fetch.
	if _, e := gitx.Run(ctx, root, "-c", "core.hooksPath=/dev/null", "fetch", "--no-tags", "--no-write-fetch-head", bundle, "+refs/*:refs/onthego/incoming/*"); e != nil {
		return restoreError("import bundled Git objects", e)
	}
	return nil
}

func validateState(ctx context.Context, root string, m snapshot.Manifest) error {
	for _, id := range []string{m.SourceGitHead, m.Git.WIPCommit, m.Git.IndexTree, m.Git.WorktreeTree, m.Git.UntrackedTree} {
		if !gitx.ValidOID(id) {
			return restoreError("invalid Git object identifier", nil)
		}
	}
	actual, e := gitx.Text(ctx, root, "rev-parse", m.Git.WIPCommit+"^{tree}", m.Git.WIPCommit+"^1", m.Git.WIPCommit+"^2^{tree}", m.Git.WIPCommit+"^3^{tree}")
	expected := strings.Join([]string{m.Git.WorktreeTree, m.SourceGitHead, m.Git.IndexTree, m.Git.UntrackedTree}, "\n")
	if e != nil || actual != expected {
		return restoreError("WIP commit does not match signed trees", e)
	}
	if m.SourceBranch != "" && m.SourceBranch != "(detached)" {
		if _, e = gitx.Run(ctx, root, "check-ref-format", "--branch", m.SourceBranch); e != nil {
			return restoreError("invalid source branch", e)
		}
	}
	if m.Git.OriginURL != "" && gitx.SafeOriginURL(m.Git.OriginURL) != m.Git.OriginURL {
		return restoreError("snapshot origin is unsafe", nil)
	}
	remaining := packageotg.MaxPayloadSize
	for _, file := range m.Files {
		if file.Secret {
			remaining -= file.SizeBytes
		}
	}
	if remaining < 0 {
		return restoreError("secret files exceed workspace payload limit", nil)
	}
	if e = gitx.CheckWorkspaceSize(ctx, root, m.SourceGitHead, []string{m.Git.IndexTree, m.Git.WorktreeTree}, m.Git.UntrackedTree, remaining); e != nil {
		return e
	}
	for _, tree := range []string{m.Git.IndexTree, m.Git.WorktreeTree, m.Git.UntrackedTree} {
		entries, e := gitx.Tree(ctx, root, tree)
		if e != nil {
			return restoreError("read incoming Git tree", e)
		}
		for _, f := range entries {
			if _, e = packageotg.ProjectPath(f.Path); e != nil {
				return restoreError("unsafe path in Git tree", e)
			}
			if f.Mode != "100644" && f.Mode != "100755" && f.Mode != "120000" {
				return restoreError("unsupported Git file mode", nil)
			}
			if f.Mode == "120000" {
				r, e := gitx.Run(ctx, root, "cat-file", "blob", f.OID)
				if e != nil {
					return e
				}
				if e = safeLink(f.Path, string(r.Stdout)); e != nil {
					return e
				}
			}
		}
	}
	extras, e := gitx.Tree(ctx, root, m.Git.UntrackedTree)
	if e != nil {
		return e
	}
	tracked, e := gitx.Tree(ctx, root, m.Git.WorktreeTree)
	if e != nil {
		return e
	}
	gitPaths := map[string]bool{}
	for _, entry := range append(tracked, extras...) {
		gitPaths[entry.Path] = true
	}
	files := map[string]snapshot.File{}
	for _, f := range m.Files {
		if f.Secret && gitPaths[f.RelativePath] {
			return packageInvalid("secret path duplicates a Git tree path: "+f.RelativePath, nil)
		}
		if !f.Secret {
			files[f.RelativePath] = f
		}
	}
	if len(extras) != len(files) {
		return restoreError("managed tree differs from file manifest", nil)
	}
	for _, entry := range extras {
		f, ok := files[entry.Path]
		if !ok {
			return restoreError("unsigned managed file in tree", nil)
		}
		r, e := gitx.Run(ctx, root, "cat-file", "blob", entry.OID)
		if e != nil {
			return e
		}
		hash := sha256.Sum256(r.Stdout)
		if hex.EncodeToString(hash[:]) != f.SHA256 || int64(len(r.Stdout)) != f.SizeBytes {
			return restoreError("managed file differs from signed metadata", nil)
		}
		if (entry.Mode == "120000") != (f.Kind == "symlink") {
			return restoreError("managed file kind differs from signed metadata", nil)
		}
		if f.Kind == "symlink" && string(r.Stdout) != f.LinkTarget {
			return restoreError("managed symlink differs from signed metadata", nil)
		}
	}
	return nil
}

// Apply changes working files in place. Existing Git configuration, hooks,
// stash, reflogs, ignored directories and unmanaged files stay at their paths.
// previousManaged contains the prior handoff's extra file names, including
// names since deleted at the sender.
func Apply(ctx context.Context, extracted, root string, m snapshot.Manifest, previousManaged []string, previousSecrets ...[]string) (_ Result, returnedErr error) {
	root, e := fsx.Canonical(root)
	if e != nil {
		return Result{}, e
	}
	fresh := false
	created := false
	existedEmpty := false
	var owned os.FileInfo
	defer func() {
		if returnedErr == nil || !created || owned == nil {
			return
		}
		current, err := os.Lstat(root)
		if err != nil || !os.SameFile(owned, current) {
			return
		}
		if existedEmpty {
			entries, err := os.ReadDir(root)
			if err == nil {
				for _, entry := range entries {
					_ = os.RemoveAll(filepath.Join(root, entry.Name()))
				}
			}
		} else {
			_ = os.RemoveAll(root)
		}
	}()
	if _, e = gitx.Run(ctx, root, "rev-parse", "--git-dir"); e != nil {
		if info, err := os.Lstat(root); err == nil {
			if !info.IsDir() {
				return Result{}, restoreError("destination is not a directory", nil)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				return Result{}, err
			}
			if len(entries) > 0 {
				return Result{}, restoreError("destination is not an empty directory or Git worktree", nil)
			}
			existedEmpty = true
		} else if !os.IsNotExist(err) {
			return Result{}, err
		}
		if e = os.MkdirAll(root, 0700); e != nil {
			return Result{}, e
		}
		owned, e = os.Lstat(root)
		if e != nil {
			return Result{}, e
		}
		created = true
		if _, e = gitx.Run(ctx, root, "init", "--quiet", objectFormatArgument(m.SourceGitHead)); e != nil {
			return Result{}, e
		}
		fresh = true
	} else if e = gitx.Validate(ctx, root); e != nil {
		return Result{}, e
	}
	if e = importBundle(ctx, root, extracted, m); e != nil {
		return Result{}, e
	}
	if e = validateState(ctx, root, m); e != nil {
		return Result{}, e
	}
	state := gitx.WIP{Head: m.SourceGitHead, Branch: m.SourceBranch, Commit: m.Git.WIPCommit, IndexTree: m.Git.IndexTree, WorktreeTree: m.Git.WorktreeTree, UntrackedTree: m.Git.UntrackedTree}
	if e = checkBranchWorktrees(ctx, root, state.Branch); e != nil {
		return Result{}, e
	}
	old, e := managedBefore(ctx, root, previousManaged)
	if e != nil {
		return Result{}, e
	}
	desired, e := desiredEntries(ctx, root, state)
	if e != nil {
		return Result{}, e
	}
	addSecretEntries(desired, m.Files)
	warnings, e := retainUnmanagedPaths(ctx, root, m.RetainedPaths, desired, old)
	if e != nil {
		return Result{}, e
	}
	if e = checkCollisions(ctx, root, old, desired); e != nil {
		return Result{}, e
	}
	var backup string
	if !fresh {
		var secretPaths []string
		for _, paths := range previousSecrets {
			secretPaths = append(secretPaths, paths...)
		}
		for _, file := range m.Files {
			if file.Secret {
				secretPaths = append(secretPaths, file.RelativePath)
			}
		}
		var backupManaged []string
		for _, path := range previousManaged {
			if old[path] {
				backupManaged = append(backupManaged, path)
			}
		}
		backup, e = createBackup(ctx, root, backupManaged, desired, secretPaths, state.Branch)
		if e != nil {
			return Result{}, e
		}
	}
	result := Result{Manifest: m, Root: root, Backup: backup, Warnings: warnings}
	var current map[string]gitx.TreeEntry
	if backup != "" {
		saved, err := readBackup(ctx, root, backup)
		if err != nil {
			return result, err
		}
		current, err = desiredEntries(ctx, root, saved.WIP)
		if err != nil {
			return result, err
		}
		addSecretEntries(current, saved.Files)
	}
	if e = applyState(ctx, root, state, m.Files, old, current, extracted); e != nil {
		if backup != "" {
			return result, &otgerror.Error{Code: otgerror.CodeRestoreFailed, Message: "in-place apply failed; restore with onthego undo", Cause: e, Preserved: backup}
		}
		return result, e
	}
	if fresh {
		// Retain portable extra branches/tags/remotes only when creating a repository.
		r, err := gitx.Run(ctx, root, "for-each-ref", "--format=%(refname) %(objectname)", "refs/onthego/incoming/heads", "refs/onthego/incoming/tags", "refs/onthego/incoming/remotes", "refs/onthego/incoming/stash")
		if err != nil {
			return result, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(r.Stdout)), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			ref := "refs/" + strings.TrimPrefix(f[0], "refs/onthego/incoming/")
			if _, err = gitx.Run(ctx, root, "update-ref", ref, f[1]); err != nil {
				return result, err
			}
		}
		if m.Git.OriginURL != "" {
			if _, e = gitx.Run(ctx, root, "remote", "add", "origin", m.Git.OriginURL); e != nil {
				return result, e
			}
		}
	}
	if e = PruneBackups(ctx, root, DefaultBackupLimit); e != nil {
		return result, e
	}
	return result, nil
}

func desiredEntries(ctx context.Context, root string, s gitx.WIP) (map[string]gitx.TreeEntry, error) {
	out := map[string]gitx.TreeEntry{}
	for _, tree := range []string{s.WorktreeTree, s.UntrackedTree} {
		entries, e := gitx.Tree(ctx, root, tree)
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			if previous, duplicate := out[entry.Path]; duplicate && (previous.Mode != entry.Mode || previous.OID != entry.OID) {
				return nil, restoreError("incoming Git trees contain conflicting duplicate path: "+entry.Path, nil)
			}
			out[entry.Path] = entry
		}
	}
	for p := range out {
		for parent := filepath.ToSlash(filepath.Dir(p)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			if _, ok := out[parent]; ok {
				return nil, restoreError("incoming file has a file or symlink ancestor: "+p, nil)
			}
		}
	}
	return out, nil
}

func managedBefore(ctx context.Context, root string, previous []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, p := range previous {
		if _, e := packageotg.ProjectPath(p); e != nil {
			return nil, e
		}
		out[p] = true
	}
	r, e := gitx.Run(ctx, root, "ls-files", "-z")
	if e != nil {
		return nil, e
	}
	for _, p := range strings.Split(string(r.Stdout), "\x00") {
		if p != "" {
			out[p] = true
		}
	}
	if head, e := gitx.Text(ctx, root, "rev-parse", "HEAD"); e == nil {
		entries, e := gitx.Tree(ctx, root, head)
		if e != nil {
			return nil, e
		}
		for _, f := range entries {
			out[f.Path] = true
		}
	}
	return out, nil
}

func checkCollisions(ctx context.Context, root string, old map[string]bool, desired map[string]gitx.TreeEntry) error {
	key, err := pathKeyer(root)
	if err != nil {
		return err
	}
	if err = checkFilesystemPaths(desired, key); err != nil {
		return err
	}
	if head, headErr := gitx.Text(ctx, root, "rev-parse", "--verify", "HEAD"); headErr == nil {
		entries, treeErr := gitx.Tree(ctx, root, head)
		if treeErr != nil {
			return treeErr
		}
		tracked := map[string]gitx.TreeEntry{}
		for _, entry := range entries {
			tracked[entry.Path] = entry
		}
		if err = checkFilesystemPaths(tracked, key); err != nil {
			return err
		}
	}
	oldKeys := map[string]bool{}
	for path := range old {
		oldKeys[key(path)] = true
	}
	// The same managed leaf may acquire a new case or normalization spelling.
	for path := range desired {
		if oldKeys[key(path)] {
			old[path] = true
		}
	}
	for path := range desired {
		if _, e := packageotg.ProjectPath(path); e != nil {
			return e
		}
		// Reject all unmanaged entries, including ignored directories, before any
		// destructive write. File-to-directory transitions of managed paths are OK.
		for p := path; p != "."; p = filepath.ToSlash(filepath.Dir(p)) {
			info, e := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
			if os.IsNotExist(e) || errors.Is(e, syscall.ENOTDIR) {
				continue
			}
			if e != nil {
				return e
			}
			if p == path {
				if info.IsDir() {
					if e = directoryManaged(root, p, old); e != nil {
						return e
					}
				} else if !old[p] {
					same, err := sameLocalBlob(ctx, root, p, desired[p])
					if err != nil {
						return err
					}
					if !same {
						return restoreError("incoming file would replace unmanaged local path (possibly left after being no longer managed): "+p, nil)
					}
					old[p] = true
				}
			} else if !info.IsDir() {
				if !old[p] {
					return restoreError("incoming path traverses unmanaged local file: "+p, nil)
				}
			}
		}
	}
	return nil
}

func applyState(ctx context.Context, root string, s gitx.WIP, files []snapshot.File, old map[string]bool, current map[string]gitx.TreeEntry, secretRoot string) error {
	desired, e := desiredEntries(ctx, root, s)
	if e != nil {
		return e
	}
	addSecretEntries(desired, files)
	key, e := pathKeyer(root)
	if e != nil {
		return e
	}
	desiredKeys := map[string]bool{}
	for path := range desired {
		desiredKeys[key(path)] = true
	}
	oldAliases := map[string][]string{}
	for path := range old {
		k := key(path)
		oldAliases[k] = append(oldAliases[k], path)
	}
	// Remove only managed leaves. Never recursively remove a local directory.
	paths := make([]string, 0, len(old))
	for p := range old {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if desiredKeys[key(p)] {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(p))
		info, err := os.Lstat(path)
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			continue
		}
		if e = packageotg.CheckParents(root, p); e != nil {
			return e
		}
		if e = os.Remove(path); e != nil {
			return e
		}
		pruneEmptyParents(root, filepath.Dir(path))
	}
	metadata := map[string]snapshot.File{}
	for _, f := range files {
		metadata[f.RelativePath] = f
	}
	paths = paths[:0]
	for p := range desired {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var written []writtenPath
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := desired[p]
		path := filepath.Join(root, filepath.FromSlash(p))
		if info, err := os.Lstat(path); err == nil {
			if err = checkWrittenAlias(p, info, written); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) {
			return err
		}
		renamesAlias := false
		for _, previous := range oldAliases[key(p)] {
			if previous != p {
				renamesAlias = true
				break
			}
		}
		// Equal bytes do not make a case/normalization-only rename a no-op.
		if prior, ok := current[p]; ok && !renamesAlias && prior.OID == entry.OID && prior.Mode == entry.Mode {
			if f, ok := metadata[p]; ok && f.Kind != "symlink" {
				mode := os.FileMode(f.Mode) & 0777
				if f.Secret {
					mode = 0600
				}
				if mode == 0 {
					mode = 0600
				}
				info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
				if err != nil {
					return err
				}
				if info.Mode().IsRegular() && info.Mode().Perm() != mode {
					if err = os.Chmod(filepath.Join(root, filepath.FromSlash(p)), mode); err != nil {
						return err
					}
				}
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			written = append(written, writtenPath{path: p, info: info})
			continue
		}
		if e = packageotg.CheckParents(root, p); e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return e
		}
		var content []byte
		if f, ok := metadata[p]; ok && f.Secret {
			content, e = os.ReadFile(filepath.Join(secretRoot, filepath.FromSlash(f.ArchivePath)))
			if e == nil {
				hash := sha256.Sum256(content)
				if hex.EncodeToString(hash[:]) != f.SHA256 {
					e = restoreError("secret payload hash mismatch", nil)
				}
			}
		} else {
			var r gitx.Result
			r, e = gitx.Run(ctx, root, "cat-file", "blob", entry.OID)
			content = r.Stdout
		}
		if e != nil {
			return e
		}
		if entry.Mode == "120000" {
			if e = safeLink(p, string(content)); e != nil {
				return e
			}
		}
		mode := os.FileMode(0644)
		if entry.Mode == "100755" {
			mode = 0755
		}
		if f, ok := metadata[p]; ok {
			mode = os.FileMode(f.Mode) & 0777
			if f.Secret {
				mode = 0600
			}
			if mode == 0 {
				mode = 0600
			}
		}
		// On case-insensitive filesystems replacing an existing directory entry
		// can preserve its old spelling. Rename the managed alias first, which
		// atomically changes only its name and keeps its contents intact.
		for _, previous := range oldAliases[key(p)] {
			if previous != p {
				previousPath := filepath.Join(root, filepath.FromSlash(previous))
				if _, err := os.Lstat(previousPath); err == nil {
					if e = os.Rename(previousPath, path); e != nil {
						return e
					}
				} else if !os.IsNotExist(err) {
					return err
				}
				break
			}
		}
		if e = replaceFile(path, content, mode, entry.Mode == "120000"); e != nil {
			return e
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if err = checkWrittenAlias(p, info, written); err != nil {
			return err
		}
		written = append(written, writtenPath{path: p, info: info})
	}
	if _, e = gitx.Run(ctx, root, "read-tree", s.IndexTree); e != nil {
		return e
	}
	if s.Branch != "" && s.Branch != "(detached)" {
		ref := "refs/heads/" + s.Branch
		if _, e = gitx.Run(ctx, root, "update-ref", "-m", "onthego handoff", ref, s.Head); e != nil {
			return e
		}
		if _, e = gitx.Run(ctx, root, "symbolic-ref", "-m", "onthego handoff", "HEAD", ref); e != nil {
			return e
		}
	} else {
		if _, e = gitx.Run(ctx, root, "update-ref", "--no-deref", "-m", "onthego handoff", "HEAD", s.Head); e != nil {
			return e
		}
	}
	return nil
}

func checkBranchWorktrees(ctx context.Context, root, branch string) error {
	if branch == "" || branch == "(detached)" {
		return nil
	}
	data, e := gitx.Run(ctx, root, "worktree", "list", "--porcelain", "-z")
	if e != nil {
		return e
	}
	var current string
	for _, line := range strings.Split(string(data.Stdout), "\x00") {
		if strings.HasPrefix(line, "worktree ") {
			current = strings.TrimPrefix(line, "worktree ")
			current, _ = fsx.Canonical(current)
		}
		if line == "branch refs/heads/"+branch && current != root {
			return restoreError("incoming branch is checked out in another linked worktree: "+current, nil)
		}
	}
	return nil
}

func safeLink(path, target string) error {
	if target == "" || filepath.IsAbs(target) || strings.ContainsAny(target, "\\\x00") || (len(target) > 1 && target[1] == ':') {
		return restoreError("unsafe Git symlink: "+path, nil)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(path)), target))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return restoreError("Git symlink escapes project: "+path, nil)
	}
	return nil
}

func pruneEmptyParents(root, path string) {
	for path != root && path != "." {
		if e := os.Remove(path); e != nil {
			return
		}
		path = filepath.Dir(path)
	}
}

func tempDir(prefix string) (string, error) {
	home, e := config.DataRoot()
	if e != nil {
		return "", e
	}
	return fsx.TempDir(home, prefix)
}
func restoreError(message string, err error) error {
	return &otgerror.Error{Code: otgerror.CodeRestoreFailed, Message: message, Cause: err, Stage: "restore"}
}

func directoryManaged(root, relative string, old map[string]bool) error {
	return filepath.WalkDir(filepath.Join(root, filepath.FromSlash(relative)), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		p, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		p = filepath.ToSlash(p)
		if !entry.IsDir() {
			if !old[p] {
				return restoreError("incoming file would replace unmanaged local path (possibly left after being no longer managed): "+p, nil)
			}
			return nil
		}
		prefix := p + "/"
		for managed := range old {
			if strings.HasPrefix(managed, prefix) {
				return nil
			}
		}
		return restoreError("incoming file would replace unmanaged local directory: "+p, nil)
	})
}

func sameLocalBlob(ctx context.Context, root, path string, entry gitx.TreeEntry) (bool, error) {
	p := filepath.Join(root, filepath.FromSlash(path))
	if strings.HasPrefix(entry.OID, "secret:") {
		info, e := os.Lstat(p)
		if e != nil {
			return false, e
		}
		var data []byte
		if info.Mode()&os.ModeSymlink != 0 {
			if entry.Mode != "120000" {
				return false, nil
			}
			target, e := os.Readlink(p)
			if e != nil {
				return false, e
			}
			data = []byte(target)
		} else {
			if !info.Mode().IsRegular() || entry.Mode == "120000" {
				return false, nil
			}
			data, e = os.ReadFile(p)
			if e != nil {
				return false, e
			}
		}
		hash := sha256.Sum256(data)
		return hex.EncodeToString(hash[:]) == strings.TrimPrefix(entry.OID, "secret:"), nil
	}
	info, e := os.Lstat(p)
	if e != nil {
		return false, e
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if entry.Mode != "120000" {
			return false, nil
		}
		target, e := os.Readlink(p)
		if e != nil {
			return false, e
		}
		r, e := gitx.RunInput(ctx, root, nil, []byte(target), "hash-object", "--stdin")
		return strings.TrimSpace(string(r.Stdout)) == entry.OID, e
	}
	if !info.Mode().IsRegular() || entry.Mode == "120000" {
		return false, nil
	}
	actual, e := gitx.Text(ctx, root, "hash-object", "--no-filters", "--", path)
	return actual == entry.OID, e
}

func addSecretEntries(entries map[string]gitx.TreeEntry, files []snapshot.File) {
	for _, f := range files {
		if !f.Secret {
			continue
		}
		mode := "100644"
		if f.Kind == "symlink" {
			mode = "120000"
		} else if f.Mode&0111 != 0 {
			mode = "100755"
		}
		entries[f.RelativePath] = gitx.TreeEntry{Mode: mode, OID: "secret:" + f.SHA256, Path: f.RelativePath}
	}
}

func objectFormatArgument(head string) string {
	if len(head) == 64 {
		return "--object-format=sha256"
	}
	return "--object-format=sha1"
}

func checkTreeParents(entries map[string]gitx.TreeEntry) error {
	for path := range entries {
		for p := filepath.ToSlash(filepath.Dir(path)); p != "."; p = filepath.ToSlash(filepath.Dir(p)) {
			if _, ok := entries[p]; ok {
				return restoreError("incoming file has a file or symlink ancestor: "+path, nil)
			}
		}
	}
	return nil
}
