package gitx

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/peerproblem/onthego/internal/otgerror"
)

// SafeOriginURL retains portable remotes while removing embedded credentials.
// Local paths and Git's executable remote helpers are deliberately omitted.
func SafeOriginURL(raw string) string {
	if strings.ContainsAny(raw, "\r\n\x00") || strings.HasPrefix(raw, "-") {
		return ""
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http" && parsed.Scheme != "ssh" && parsed.Scheme != "git") {
			return ""
		}
		if parsed.Scheme != "ssh" {
			parsed.User = nil
		} else if parsed.User != nil {
			parsed.User = url.User(parsed.User.Username())
		}
		parsed.RawQuery, parsed.Fragment = "", ""
		return parsed.String()
	}
	if colon := strings.IndexByte(raw, ':'); colon > 0 && colon < len(raw)-1 && !strings.ContainsAny(raw[:colon], "/\\ ") && !strings.Contains(raw, "::") {
		return raw
	}
	return ""
}

type Result struct {
	Stdout []byte
	Stderr []byte
}

func Run(ctx context.Context, root string, args ...string) (Result, error) {
	return run(ctx, root, nil, nil, args...)
}

func run(ctx context.Context, root string, env []string, input []byte, args ...string) (Result, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	command.Env = append(command.Env, env...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

func Version(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, "git", "--version")
	data, err := command.Output()
	return strings.TrimSpace(string(data)), err
}

func Validate(ctx context.Context, root string) error {
	short, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	inside, err := Run(short, root, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(inside.Stdout)) != "true" {
		return otgerror.Wrap(otgerror.CodeUnsupportedGit, "not a supported Git worktree", err)
	}
	if _, err := Run(short, root, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
		return otgerror.Wrap(otgerror.CodeUnsupportedGit, "repository must have a HEAD commit", err)
	}
	shallow, err := Run(short, root, "rev-parse", "--is-shallow-repository")
	if err == nil && strings.TrimSpace(string(shallow.Stdout)) == "true" {
		return otgerror.New(otgerror.CodeUnsupportedGit, "shallow repositories are not supported")
	}
	unmerged, err := Run(short, root, "ls-files", "-u", "-z")
	if err != nil {
		return err
	}
	if len(unmerged.Stdout) > 0 {
		return otgerror.New(otgerror.CodeUnsupportedGit, "repositories with unmerged index entries are not supported")
	}
	flags, err := Run(short, root, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for _, entry := range bytes.Split(flags.Stdout, []byte{0}) {
		if len(entry) > 0 && (entry[0] == 'S' || (entry[0] >= 'a' && entry[0] <= 'z')) {
			return otgerror.New(otgerror.CodeUnsupportedGit, "clear assume-unchanged and skip-worktree index flags before handoff")
		}
	}
	visible, err := Run(short, root, "diff", "--cached", "--raw", "-z", "--ita-visible-in-index")
	if err != nil {
		return err
	}
	invisible, err := Run(short, root, "diff", "--cached", "--raw", "-z", "--ita-invisible-in-index")
	if err != nil {
		return err
	}
	if !bytes.Equal(visible.Stdout, invisible.Stdout) {
		return otgerror.New(otgerror.CodeUnsupportedGit, "stage or reset intent-to-add index entries before handoff")
	}
	if _, err := os.Stat(root + "/.gitmodules"); err == nil {
		return otgerror.New(otgerror.CodeUnsupportedGit, "submodule handoff is not supported")
	}
	sparse, err := Run(short, root, "config", "--bool", "core.sparseCheckout")
	if err == nil && strings.TrimSpace(string(sparse.Stdout)) == "true" {
		return otgerror.New(otgerror.CodeUnsupportedGit, "sparse checkout handoff is not supported")
	}
	partial, err := Run(short, root, "config", "--local", "--get-regexp", `^(extensions\.partialClone|remote\..*\.promisor)$`)
	if err == nil && len(strings.TrimSpace(string(partial.Stdout))) > 0 {
		return otgerror.New(otgerror.CodeUnsupportedGit, "partial clone handoff is not supported")
	}
	filters, err := Run(short, root, "config", "--local", "--get-regexp", `^filter\.`)
	if err == nil && len(strings.TrimSpace(string(filters.Stdout))) > 0 {
		return otgerror.New(otgerror.CodeUnsupportedGit, "repositories with Git filters are not supported")
	}
	lfs, err := Run(short, root, "grep", "-I", "-n", "filter=lfs", "--", "*.gitattributes")
	if err == nil && len(strings.TrimSpace(string(lfs.Stdout))) > 0 {
		return otgerror.New(otgerror.CodeUnsupportedGit, "Git LFS handoff is not supported")
	}
	return nil
}
