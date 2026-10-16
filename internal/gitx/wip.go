package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/peerproblem/onthego/internal/fsx"
	"golang.org/x/sys/unix"
)

// WIP stores the three independent Git trees needed for a lossless handoff.
// The first three commit parents are HEAD, the index commit and the extra-files
// commit. An optional fourth parent links the preceding handoff.
type WIP struct {
	Head          string `json:"head"`
	Branch        string `json:"branch"`
	IndexTree     string `json:"index_tree"`
	WorktreeTree  string `json:"worktree_tree"`
	UntrackedTree string `json:"untracked_tree"`
	Commit        string `json:"commit"`
}

// RunInput executes plumbing with explicit stdin and environment overrides.
func RunInput(ctx context.Context, root string, env []string, input []byte, args ...string) (Result, error) {
	return run(ctx, root, env, input, args...)
}

func Text(ctx context.Context, root string, args ...string) (string, error) {
	r, err := Run(ctx, root, args...)
	return strings.TrimSpace(string(r.Stdout)), err
}

// CaptureWIP never changes the user's index, branch or working files.
func CaptureWIP(ctx context.Context, root, temp string, extra []string, parent string) (WIP, error) {
	var s WIP
	var err error
	s.Head, err = Text(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return s, err
	}
	s.Branch, _ = Text(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if s.Branch == "" {
		s.Branch = "(detached)"
	}
	indexPath, err := Text(ctx, root, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return s, err
	}
	data, err := os.ReadFile(indexPath)
	if err != nil && !os.IsNotExist(err) {
		return s, err
	}
	f, err := os.CreateTemp(temp, "index-*")
	if err != nil {
		return s, err
	}
	index := f.Name()
	defer os.Remove(index)
	if err = f.Close(); err != nil {
		return s, err
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if len(data) > 0 {
		if err = os.WriteFile(index, data, 0600); err != nil {
			return s, err
		}
	} else {
		os.Remove(index)
		if _, err = RunInput(ctx, root, env, nil, "read-tree", "--empty"); err != nil {
			return s, err
		}
	}
	text := func(args ...string) (string, error) {
		r, e := RunInput(ctx, root, env, nil, args...)
		return strings.TrimSpace(string(r.Stdout)), e
	}
	s.IndexTree, err = text("write-tree")
	if err != nil {
		return s, err
	}
	tracked, err := Tree(ctx, root, s.IndexTree)
	if err != nil {
		return s, err
	}
	paths := make([]string, 0, len(tracked))
	for _, entry := range tracked {
		paths = append(paths, entry.Path)
	}
	s.WorktreeTree, err = rawTree(ctx, root, temp, env, paths, true)
	if err != nil {
		return s, err
	}
	s.UntrackedTree, err = rawTree(ctx, root, temp, env, extra, false)
	if err != nil {
		return s, err
	}
	indexCommit, err := CommitTree(ctx, root, s.IndexTree, []string{s.Head}, "ONTHEGO index\n")
	if err != nil {
		return s, err
	}
	extrasCommit, err := CommitTree(ctx, root, s.UntrackedTree, nil, "ONTHEGO managed files\n")
	if err != nil {
		return s, err
	}
	parents := []string{s.Head, indexCommit, extrasCommit}
	if parent != "" {
		if !ValidOID(parent) {
			return s, fmt.Errorf("invalid preceding WIP commit")
		}
		if _, e := Run(ctx, root, "cat-file", "-e", parent+"^{commit}"); e != nil {
			return s, fmt.Errorf("preceding handoff commit is unavailable: %w", e)
		}
		parents = append(parents, parent)
	}
	s.Commit, err = CommitTree(ctx, root, s.WorktreeTree, parents, "ONTHEGO WIP\n")
	return s, err
}

func CommitTree(ctx context.Context, root, tree string, parents []string, message string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	env := []string{"GIT_AUTHOR_NAME=ONTHEGO", "GIT_AUTHOR_EMAIL=handoff@onthego.local", "GIT_COMMITTER_NAME=ONTHEGO", "GIT_COMMITTER_EMAIL=handoff@onthego.local"}
	r, err := RunInput(ctx, root, env, []byte(message), args...)
	return strings.TrimSpace(string(r.Stdout)), err
}

func ValidOID(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && (len(b) == 20 || len(b) == 32)
}

func Tips(ctx context.Context, root string) ([]string, error) {
	r, e := Run(ctx, root, "for-each-ref", "--format=%(objectname)")
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range strings.Fields(string(r.Stdout)) {
		if ValidOID(s) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if h, e := Text(ctx, root, "rev-parse", "HEAD"); e == nil && !seen[h] {
		out = append(out, h)
	}
	return out, nil
}

func IsAncestor(ctx context.Context, root, ancestor, descendant string) (bool, error) {
	if !ValidOID(ancestor) || !ValidOID(descendant) {
		return false, fmt.Errorf("invalid Git object ID")
	}
	r, e := Run(ctx, root, "merge-base", "--is-ancestor", ancestor, descendant)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if e != nil && len(r.Stderr) > 0 {
		return false, e
	}
	return e == nil, nil
}

// Bundle includes portable user refs and the WIP ref, never internal backups.
// Unknown recipient tips are ignored; only locally available commits can form
// valid bundle prerequisites.
func Bundle(ctx context.Context, root, path, wip string, tips []string) error {
	h := sha256.Sum256([]byte(root))
	ref := "refs/onthego/workspaces/" + hex.EncodeToString(h[:8])
	if _, e := Run(ctx, root, "update-ref", ref, wip); e != nil {
		return e
	}
	refs, e := Run(ctx, root, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags", "refs/remotes", "refs/stash")
	if e != nil {
		return e
	}
	args := []string{"bundle", "create", path, ref}
	args = append(args, strings.Fields(string(refs.Stdout))...)
	for _, tip := range tips {
		if !ValidOID(tip) || tip == wip {
			continue
		}
		if _, e := Run(ctx, root, "cat-file", "-e", tip+"^{commit}"); e == nil {
			args = append(args, "^"+tip)
		}
	}
	_, e = Run(ctx, root, args...)
	return e
}

type TreeEntry struct {
	Mode string
	OID  string
	Path string
}

func Tree(ctx context.Context, root, tree string) ([]TreeEntry, error) {
	if !ValidOID(tree) {
		return nil, fmt.Errorf("invalid Git tree ID")
	}
	r, e := Run(ctx, root, "ls-tree", "-r", "-z", tree)
	if e != nil {
		return nil, e
	}
	var out []TreeEntry
	for _, line := range bytes.Split(r.Stdout, []byte{0}) {
		if len(line) == 0 {
			continue
		}
		p := bytes.SplitN(line, []byte{'\t'}, 2)
		if len(p) != 2 {
			return nil, fmt.Errorf("invalid tree entry")
		}
		f := strings.Fields(string(p[0]))
		if len(f) != 3 || f[1] != "blob" {
			return nil, fmt.Errorf("unsupported Git tree entry")
		}
		out = append(out, TreeEntry{f[0], f[2], string(p[1])})
	}
	return out, nil
}

func ObjectDirectory(ctx context.Context, root string) (string, error) {
	p, e := Text(ctx, root, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	return filepath.Clean(p), e
}

// CheckWorkspaceSize bounds changed file contents, separately from Git history.
func CheckWorkspaceSize(ctx context.Context, root, head string, trees []string, extra string, limit int64) error {
	base, e := Tree(ctx, root, head)
	if e != nil {
		return e
	}
	known := map[string]bool{}
	for _, f := range base {
		known[f.OID] = true
	}
	changed := map[string]bool{}
	for _, tree := range append(append([]string{}, trees...), extra) {
		files, e := Tree(ctx, root, tree)
		if e != nil {
			return e
		}
		for _, f := range files {
			if tree == extra || !known[f.OID] {
				changed[f.OID] = true
			}
		}
	}
	var ids []string
	for id := range changed {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	r, e := RunInput(ctx, root, nil, []byte(strings.Join(ids, "\n")+"\n"), "cat-file", "--batch-check=%(objectsize)")
	if e != nil {
		return e
	}
	var size int64
	for _, line := range strings.Fields(string(r.Stdout)) {
		var n int64
		if _, e = fmt.Sscan(line, &n); e != nil || n < 0 {
			return fmt.Errorf("invalid Git object size")
		}
		size += n
		if size > limit {
			return fmt.Errorf("workspace changes exceed the 250 MiB payload limit")
		}
	}
	return nil
}

// rawTree records filesystem bytes without Git attributes, EOL conversion or
// clean filters. The staged tree above remains the user's original index.
func rawTree(ctx context.Context, root, temp string, env, paths []string, missingOK bool) (string, error) {
	if _, e := RunInput(ctx, root, env, nil, "read-tree", "--empty"); e != nil {
		return "", e
	}
	// Bound open descriptors and subprocess count. /dev/fd paths contain no user
	// filenames, so --stdin-paths never needs ambiguous C-style filename quoting.
	const batchSize = 64
	var files []*os.File
	var names, modes []string
	closeFiles := func() {
		for _, f := range files {
			_ = f.Close()
		}
		files = nil
		names = nil
		modes = nil
	}
	defer closeFiles()
	flush := func() error {
		if len(files) == 0 {
			return nil
		}
		ids, e := hashRawBatch(ctx, root, files)
		if e != nil {
			return e
		}
		var updates bytes.Buffer
		for i, id := range ids {
			fmt.Fprintf(&updates, "%s %s\t%s\x00", modes[i], id, names[i])
		}
		if _, e = RunInput(ctx, root, env, updates.Bytes(), "update-index", "-z", "--index-info"); e != nil {
			return e
		}
		closeFiles()
		return nil
	}
	for _, path := range paths {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		absolute := filepath.Join(root, filepath.FromSlash(path))
		info, e := os.Lstat(absolute)
		if missingOK && (os.IsNotExist(e) || errors.Is(e, syscall.ENOTDIR)) {
			continue
		}
		if e != nil {
			return "", e
		}
		if e = fsx.CheckParents(root, path); e != nil {
			return "", e
		}
		if info.IsDir() && missingOK {
			continue
		}
		var file *os.File
		mode := "100644"
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(absolute)
			if e != nil {
				return "", e
			}
			file, e = os.CreateTemp(temp, "link-blob-")
			if e != nil {
				return "", e
			}
			_ = os.Remove(file.Name())
			if _, e = file.WriteString(target); e != nil {
				file.Close()
				return "", e
			}
			if _, e = file.Seek(0, 0); e != nil {
				file.Close()
				return "", e
			}
			mode = "120000"
		} else {
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("unsupported worktree file: %s", path)
			}
			fd, e := unix.Open(absolute, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
			if e != nil {
				return "", e
			}
			file = os.NewFile(uintptr(fd), absolute)
			opened, e := file.Stat()
			if e != nil {
				file.Close()
				return "", e
			}
			if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
				file.Close()
				return "", fmt.Errorf("worktree file changed during capture: %s", path)
			}
			if info.Mode().Perm()&0111 != 0 {
				mode = "100755"
			}
		}
		files = append(files, file)
		names = append(names, path)
		modes = append(modes, mode)
		if len(files) == batchSize {
			if e = flush(); e != nil {
				return "", e
			}
		}
	}
	if e := flush(); e != nil {
		return "", e
	}
	r, e := RunInput(ctx, root, env, nil, "write-tree")
	return strings.TrimSpace(string(r.Stdout)), e
}

func hashRawBatch(ctx context.Context, root string, files []*os.File) ([]string, error) {
	command := exec.CommandContext(ctx, "git", "hash-object", "-w", "--no-filters", "--stdin-paths")
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	command.ExtraFiles = files
	var paths strings.Builder
	for i := range files {
		fmt.Fprintf(&paths, "/dev/fd/%d\n", i+3)
	}
	command.Stdin = strings.NewReader(paths.String())
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if e := command.Run(); e != nil {
		return nil, fmt.Errorf("hash raw Git blobs: %w: %s", e, strings.TrimSpace(stderr.String()))
	}
	ids := strings.Fields(stdout.String())
	if len(ids) != len(files) {
		return nil, fmt.Errorf("Git raw blob batch returned an unexpected result count")
	}
	for _, id := range ids {
		if !ValidOID(id) {
			return nil, fmt.Errorf("Git raw blob batch returned an invalid object ID")
		}
	}
	return ids, nil
}
