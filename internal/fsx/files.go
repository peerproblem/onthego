package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open regular file", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	return file, nil
}

// CopyRegular refuses source symlinks and existing destinations. Parent path
// safety belongs to the caller's CheckParents check within its chosen root.
func CopyRegular(source, destination string, mode fs.FileMode) error {
	input, err := openRegular(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	if err := output.Chmod(mode.Perm()); err != nil {
		_ = output.Close()
		_ = os.Remove(destination)
		return err
	}
	_, copyErr := io.Copy(output, input)
	err = errors.Join(copyErr, output.Close())
	if err != nil {
		_ = os.Remove(destination)
	}
	return err
}

// HashRegular hashes a regular file without following its final symlink.
func HashRegular(path string) (string, int64, error) {
	file, err := openRegular(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	return hex.EncodeToString(hash.Sum(nil)), size, err
}
