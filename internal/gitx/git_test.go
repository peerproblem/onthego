package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSafeOriginURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://token:password@github.com/user/repo.git?token=secret#secret", "https://github.com/user/repo.git"},
		{"ssh://git:password@github.com/user/repo.git", "ssh://git@github.com/user/repo.git"},
		{"git@github.com:user/repo.git", "git@github.com:user/repo.git"},
		{"/Users/me/code", ""},
		{"file:///tmp/repo", ""},
		{"ext::sh -c command", ""},
		{"--upload-pack=command", ""},
	} {
		if got := SafeOriginURL(test.input); got != test.want {
			t.Errorf("SafeOriginURL(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestValidateRejectsHiddenOrIncompleteIndexChanges(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree", "intent-to-add"} {
		t.Run(flag, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				if _, err := Run(context.Background(), root, args...); err != nil {
					t.Fatal(err)
				}
			}
			git("init")
			file := filepath.Join(root, "tracked.txt")
			if err := os.WriteFile(file, []byte("base"), 0o600); err != nil {
				t.Fatal(err)
			}
			git("add", "tracked.txt")
			git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture")
			if flag == "intent-to-add" {
				if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new work"), 0o600); err != nil {
					t.Fatal(err)
				}
				git("add", "-N", "new.txt")
			} else {
				git("update-index", flag, "tracked.txt")
				if err := os.WriteFile(file, []byte("otherwise silently lost"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := Validate(context.Background(), root); err == nil {
				t.Fatalf("unsupported index state %s was accepted", flag)
			}
		})
	}
}
