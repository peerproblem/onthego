package fsx

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbaehova/onthego/internal/otgerror"
)

func TestCanonicalResolvesAliasesAndMissingSuffix(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink("real", alias); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "missing", "missing/deeper/file"} {
		got, err := Canonical(filepath.Join(alias, suffix))
		if err != nil || got != filepath.Join(physical, suffix) {
			t.Fatalf("Canonical(alias/%s) = %q, %v", suffix, got, err)
		}
	}
	// These aliases are present on macOS, while Linux exercises ordinary roots.
	for _, path := range []string{os.TempDir(), "/tmp", "/var"} {
		want, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		got, err := Canonical(filepath.Join(path, "onthego-nonexistent-path", "leaf"))
		if err != nil || got != filepath.Join(want, "onthego-nonexistent-path", "leaf") {
			t.Fatalf("Canonical(%s) = %q, %v", path, got, err)
		}
	}
}

func TestCanonicalRejectsBrokenSymlinkAndFileAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(root, "broken")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", filepath.Join(root, "broken", "leaf"), filepath.Join(root, "file", "leaf")} {
		if _, err := Canonical(path); err == nil {
			t.Fatalf("accepted invalid path %q", path)
		}
	}
}

func TestPathBoundaryAllowsRootAliasButRejectsInternalSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if err := CheckPath(alias, "new/file"); err != nil {
		t.Fatalf("rejected selected root alias: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := CheckParents(alias, "escape/file"); err == nil {
		t.Fatal("accepted symlink ancestor")
	}
	if err := CheckPath(alias, "escape"); err == nil {
		t.Fatal("accepted symlink leaf")
	}
	if err := CheckParents(alias, "escape"); err != nil {
		t.Fatalf("symlink leaf cannot be copied as a link: %v", err)
	}
	for _, rel := range []string{"../outside/file", "/absolute", "a/../file", "a\\file", "", "a//file"} {
		if err := CheckParents(root, rel); err == nil {
			t.Fatalf("accepted relative path %q", rel)
		}
	}
}

func TestLockRetainsInodeAndAllowsRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Lock(path)
	var typed *otgerror.Error
	if !errors.As(err, &typed) || typed.Code != otgerror.CodePrecondition || !strings.Contains(err.Error(), "capture.lock") {
		t.Fatalf("lock contention lacks actionable path: %v", err)
	}
	unlock()
	unlock()
	unlock, err = Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("lock inode replaced on release")
	}
}

func TestLockRejectsSymlinkAndNonRegularFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "link")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{alias, root} {
		if release, err := Lock(path); err == nil {
			release()
			t.Fatalf("accepted unsafe lock %s", path)
		}
	}
	data, _ := os.ReadFile(target)
	if string(data) != "private" {
		t.Fatal("lock changed symlink target")
	}
}

func TestProcessDeathReleasesLockAndExpiredTemp(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "capture.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFSXHelperProcess$")
	cmd.Env = append(os.Environ(), "ONTHEGO_FSX_HELPER=1", "ONTHEGO_FSX_HOME="+home)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- strings.TrimSpace(line) }()
	var dir string
	select {
	case dir = <-ready:
		if dir == "" {
			t.Fatal("helper exited before acquiring lock")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not acquire lock")
	}
	if unlock, err := Lock(path); err == nil {
		unlock()
		t.Fatal("cross-process contention allowed")
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := CleanupTemps(home, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("active temporary payload removed: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	unlock, err := Lock(path)
	if err != nil {
		t.Fatalf("kernel did not release killed process's lock: %v", err)
	}
	unlock()
	if err := CleanupTemps(home, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dead process's expired payload remains: %v", err)
	}
}

func TestFSXHelperProcess(t *testing.T) {
	if os.Getenv("ONTHEGO_FSX_HELPER") != "1" {
		return
	}
	home := os.Getenv("ONTHEGO_FSX_HOME")
	unlock, err := Lock(filepath.Join(home, "capture.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	dir, err := TempDir(home, "capture-")
	if err != nil {
		t.Fatal(err)
	}
	defer ReleaseTemp(dir)
	fmt.Println(dir)
	for {
		time.Sleep(time.Minute)
	}
}

func TestTempDirectoriesArePrivateAndDoNotContainLeaseFiles(t *testing.T) {
	home := t.TempDir()
	dir, err := TempDir(home, "capture-")
	if err != nil {
		t.Fatal(err)
	}
	defer ReleaseTemp(dir)
	physical, _ := Canonical(home)
	if filepath.Dir(dir) != filepath.Join(physical, "tmp") {
		t.Fatalf("temporary directory is outside private temp root: %s", dir)
	}
	for _, path := range []string{physical, filepath.Dir(dir), dir} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("temporary directory mode: %s, %v", path, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("lease leaked into payload: %v, %v", entries, err)
	}
	if err := ReleaseTemp(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("temporary directory retained after release")
	}
}

func TestCleanupDoesNotFollowSymlinksOrRemoveUnknownDirectories(t *testing.T) {
	home := t.TempDir()
	dir, err := TempDir(home, "init-")
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseTemp(dir); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	unknown := filepath.Join(home, "tmp", "unknown")
	if err := os.Mkdir(unknown, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "tmp", "alias")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(unknown, old, old); err != nil {
		t.Fatal(err)
	}
	if err := CleanupTemps(home, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{unknown, outside} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unowned path removed: %v", err)
		}
	}
}

func TestRegularFileHelpersRejectSymlinks(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := HashRegular(link); err == nil {
		t.Fatal("hashed a symlink")
	}
	if err := CopyRegular(link, filepath.Join(root, "copy"), 0o600); err == nil {
		t.Fatal("copied a symlink as a regular file")
	}
	destination := filepath.Join(root, "copy")
	if err := CopyRegular(source, destination, 0o755); err != nil {
		t.Fatal(err)
	}
	got, size, err := HashRegular(destination)
	if err != nil || got != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" || size != 5 {
		t.Fatalf("copied bytes changed: %q %d %v", got, size, err)
	}
	if err := CopyRegular(source, destination, 0o600); err == nil {
		t.Fatal("overwrote existing destination")
	}
}
