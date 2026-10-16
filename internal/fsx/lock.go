package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/peerproblem/onthego/internal/otgerror"
	"golang.org/x/sys/unix"
)

// Lock takes an exclusive, nonblocking kernel lock. Its file is deliberately
// retained: removing it could let another process lock a different inode while
// a waiting process still owns the original inode. Process exit releases flock.
// Callers must protect private parent directories against untrusted mutations.
func Lock(path string) (func(), error) {
	return lockFile(path, true)
}

func lockFile(path string, nonblocking bool) (func(), error) {
	parent, err := Canonical(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open handoff lock %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("handoff lock must be a regular file: %s", path)
	}
	operation := unix.LOCK_EX
	if nonblocking {
		operation |= unix.LOCK_NB
	}
	for {
		err = unix.Flock(fd, operation)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, otgerror.New(otgerror.CodePrecondition, "another handoff is active; retry after it finishes (lock: "+path+")")
		}
		return nil, fmt.Errorf("acquire handoff lock %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.Flock(fd, unix.LOCK_UN)
			_ = file.Close()
		})
	}, nil
}
