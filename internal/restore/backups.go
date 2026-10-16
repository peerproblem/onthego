package restore

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
	"strings"
	"syscall"
	"time"

	"github.com/peerproblem/onthego/internal/fsx"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/packageotg"
	"github.com/peerproblem/onthego/internal/snapshot"
)

func backupPrefix(root string) string {
	canonical, e := fsx.Canonical(root)
	if e == nil {
		root = canonical
	}
	h := sha256.Sum256([]byte(root))
	return "refs/onthego/backup/" + hex.EncodeToString(h[:8]) + "/"
}

func createBackup(ctx context.Context, root string, previous []string, after map[string]gitx.TreeEntry, secrets []string, targetBranch string, additionalRefs ...string) (string, error) {
	key, e := pathKeyer(root)
	if e != nil {
		return "", e
	}
	temp, e := tempDir("backup-")
	if e != nil {
		return "", e
	}
	defer fsx.ReleaseTemp(temp)
	tracked, e := gitx.Run(ctx, root, "ls-files", "-z")
	if e != nil {
		return "", e
	}
	inIndex := map[string]bool{}
	for _, p := range strings.Split(string(tracked.Stdout), "\x00") {
		inIndex[key(p)] = true
	}
	// A path removed from the index and recreated in the worktree is untracked.
	// It still needs backing up when incoming tracked changes will replace it.
	head, e := gitx.Text(ctx, root, "rev-parse", "HEAD")
	if e != nil {
		return "", e
	}
	entries, e := gitx.Tree(ctx, root, head)
	if e != nil {
		return "", e
	}
	candidates := append([]string(nil), previous...)
	for p := range after {
		if !inIndex[key(p)] {
			candidates = append(candidates, p)
		}
	}
	for _, f := range entries {
		if !inIndex[key(f.Path)] {
			candidates = append(candidates, f.Path)
		}
	}
	secretSet := map[string]bool{}
	for _, p := range secrets {
		secretSet[key(p)] = true
	}
	seen := map[string]bool{}
	var paths []string
	var files []snapshot.File
	for _, p := range candidates {
		k := key(p)
		// CaptureWIP already records indexed files with the index's spelling.
		// Supplemental aliases must not add a second copy to the extra tree.
		if seen[k] || inIndex[k] {
			continue
		}
		seen[k] = true
		if _, e = packageotg.ProjectPath(p); e != nil {
			return "", e
		}
		path := filepath.Join(root, filepath.FromSlash(p))
		info, err := os.Lstat(path)
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			continue
		}
		if e = packageotg.CheckParents(root, p); e != nil {
			return "", e
		}
		// Lstat accepts aliases on some filesystems. Keep the actual existing
		// directory-entry spelling so undo can restore it after a rename.
		p, e = existingPathSpelling(root, p, key)
		if e != nil {
			return "", e
		}
		path = filepath.Join(root, filepath.FromSlash(p))
		f := snapshot.File{RelativePath: p, Mode: uint32(info.Mode().Perm()), Kind: "file", SizeBytes: info.Size()}
		if info.Mode()&os.ModeSymlink != 0 {
			f.Kind = "symlink"
			f.LinkTarget, err = os.Readlink(path)
			if err != nil {
				return "", err
			}
			if err = safeLink(p, f.LinkTarget); err != nil {
				return "", err
			}
		}
		if secretSet[k] {
			f.Secret = true
			f.ArchivePath = "secret-files/" + p
		} else {
			paths = append(paths, p)
		}
		files = append(files, f)
	}
	s, e := gitx.CaptureWIP(ctx, root, temp, paths, "")
	if e != nil {
		return "", e
	}
	backup := backupState{WIP: s, Files: files, SecretPaths: secrets, CreatedAt: time.Now().UTC()}
	backup.BranchRefs = map[string]string{}
	refs := append([]string(nil), additionalRefs...)
	if targetBranch != "" && targetBranch != "(detached)" {
		refs = append(refs, "refs/heads/"+targetBranch)
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "refs/heads/") {
			return "", fmt.Errorf("invalid backup branch ref")
		}
		if _, e = gitx.Run(ctx, root, "check-ref-format", ref); e != nil {
			return "", e
		}
		backup.BranchRefs[ref], _ = gitx.Text(ctx, root, "rev-parse", "--verify", ref)
	}
	if e = saveSecretBackup(ctx, root, &backup); e != nil {
		return "", e
	}
	published := false
	defer func() {
		if !published {
			_ = removeSecretBackup(backup)
		}
	}()
	for p := range after {
		backup.AfterPaths = append(backup.AfterPaths, p)
	}
	sort.Strings(backup.AfterPaths)
	body, e := json.Marshal(backup)
	if e != nil {
		return "", e
	}
	parents := []string{s.Commit}
	parentSet := map[string]bool{s.Commit: true, s.Head: true}
	for _, tip := range backup.BranchRefs {
		if tip != "" && !parentSet[tip] {
			parents = append(parents, tip)
			parentSet[tip] = true
		}
	}
	sort.Strings(parents[1:])
	commit, e := gitx.CommitTree(ctx, root, s.WorktreeTree, parents, string(body)+"\n")
	if e != nil {
		return "", e
	}
	ref := backupPrefix(root) + backup.CreatedAt.Format("20060102T150405.000000000Z")
	_, e = gitx.Run(ctx, root, "update-ref", ref, commit)
	published = e == nil
	return ref, e
}

// Backups lists only this worktree's object backups, newest first.
func Backups(ctx context.Context, root string) ([]Backup, error) {
	r, e := gitx.Run(ctx, root, "for-each-ref", "--sort=-refname", "--format=%(refname) %(objectname)", backupPrefix(root))
	if e != nil {
		return nil, e
	}
	out := []Backup{}
	for _, line := range strings.Split(strings.TrimSpace(string(r.Stdout)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		b, e := readBackup(ctx, root, f[0])
		if e != nil {
			return nil, e
		}
		out = append(out, Backup{Ref: f[0], Commit: f[1], CreatedAt: b.CreatedAt, Head: b.WIP.Head, Branch: b.WIP.Branch})
	}
	return out, nil
}

func readBackup(ctx context.Context, root, ref string) (backupState, error) {
	if !strings.HasPrefix(ref, backupPrefix(root)) || strings.ContainsAny(ref, "\x00\r\n") {
		return backupState{}, fmt.Errorf("backup ref does not belong to this worktree")
	}
	r, e := gitx.Run(ctx, root, "show", "-s", "--format=%B", ref)
	if e != nil {
		return backupState{}, e
	}
	var b backupState
	if e = json.Unmarshal(r.Stdout, &b); e != nil {
		return b, e
	}
	if !gitx.ValidOID(b.WIP.Commit) || !gitx.ValidOID(b.WIP.Head) || !gitx.ValidOID(b.WIP.IndexTree) || !gitx.ValidOID(b.WIP.WorktreeTree) || !gitx.ValidOID(b.WIP.UntrackedTree) {
		return b, fmt.Errorf("invalid backup metadata")
	}
	for ref, tip := range b.BranchRefs {
		if !strings.HasPrefix(ref, "refs/heads/") {
			return b, fmt.Errorf("invalid backup branch ref")
		}
		if _, e = gitx.Run(ctx, root, "check-ref-format", ref); e != nil {
			return b, e
		}
		if tip != "" && !gitx.ValidOID(tip) {
			return b, fmt.Errorf("invalid backup branch tip")
		}
	}
	return b, nil
}

// PruneBackups removes references only. Normal Git garbage collection reclaims
// unreachable objects; local stashes and all user refs are left untouched.
func PruneBackups(ctx context.Context, root string, keep int) error {
	if keep < 0 {
		return fmt.Errorf("backup retention must be zero or greater")
	}
	backups, e := Backups(ctx, root)
	if e != nil {
		return e
	}
	for i := keep; i < len(backups); i++ {
		state, err := readBackup(ctx, root, backups[i].Ref)
		if err != nil {
			return err
		}
		if state.SecretArchive != "" {
			if _, e = secretArchivePath(state.SecretArchive); e != nil {
				return e
			}
		}
		if _, e = gitx.Run(ctx, root, "update-ref", "-d", backups[i].Ref, backups[i].Commit); e != nil {
			return e
		}
		if e = removeSecretBackup(state); e != nil {
			return e
		}
	}
	return nil
}

// Undo restores a selected object backup, or the newest one for an empty ref.
// The state being replaced is backed up too, so undo itself remains reversible.
func Undo(ctx context.Context, root, ref string) (Result, error) {
	root, e := fsx.Canonical(root)
	if e != nil {
		return Result{}, e
	}
	if ref == "" {
		list, e := Backups(ctx, root)
		if e != nil {
			return Result{}, e
		}
		if len(list) == 0 {
			return Result{}, fmt.Errorf("no ONTHEGO backups for this worktree")
		}
		ref = list[0].Ref
	}
	b, e := readBackup(ctx, root, ref)
	if e != nil {
		return Result{}, e
	}
	if e = checkBranchWorktrees(ctx, root, b.WIP.Branch); e != nil {
		return Result{}, e
	}
	var additionalRefs []string
	for ref := range b.BranchRefs {
		if e = checkBranchWorktrees(ctx, root, strings.TrimPrefix(ref, "refs/heads/")); e != nil {
			return Result{}, e
		}
		additionalRefs = append(additionalRefs, ref)
	}
	old, e := managedBefore(ctx, root, b.AfterPaths)
	if e != nil {
		return Result{}, e
	}
	desired, e := desiredEntries(ctx, root, b.WIP)
	if e != nil {
		return Result{}, e
	}
	addSecretEntries(desired, b.Files)
	if e = checkCollisions(ctx, root, old, desired); e != nil {
		return Result{}, e
	}
	secretRoot, e := loadSecretBackup(ctx, b)
	if e != nil {
		return Result{}, e
	}
	if secretRoot != "" {
		defer fsx.ReleaseTemp(secretRoot)
	}
	secrets := b.SecretPaths
	inverse, e := createBackup(ctx, root, b.AfterPaths, desired, secrets, b.WIP.Branch, additionalRefs...)
	if e != nil {
		return Result{}, e
	}
	result := Result{Root: root, Backup: inverse}
	saved, e := readBackup(ctx, root, inverse)
	if e != nil {
		return result, e
	}
	current, e := desiredEntries(ctx, root, saved.WIP)
	if e != nil {
		return result, e
	}
	addSecretEntries(current, saved.Files)
	if e = applyState(ctx, root, b.WIP, b.Files, old, current, secretRoot); e != nil {
		return result, &otgerror.Error{Code: otgerror.CodeRestoreFailed, Message: "in-place undo failed; restore with onthego undo", Cause: e, Preserved: inverse}
	}
	for branch, tip := range b.BranchRefs {
		args := []string{"update-ref", branch, tip}
		if tip == "" {
			args = []string{"update-ref", "-d", branch}
		}
		if _, e = gitx.Run(ctx, root, args...); e != nil {
			return result, e
		}
	}
	if _, e = gitx.Run(ctx, root, "update-ref", "-d", ref); e != nil {
		return result, e
	}
	if e = removeSecretBackup(b); e != nil {
		return result, e
	}
	return result, PruneBackups(ctx, root, DefaultBackupLimit)
}
