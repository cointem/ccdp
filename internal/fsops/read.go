package fsops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenAuthorizedRegular resolves legitimate aliases before authorization, then
// opens every canonical component relative to a pinned directory descriptor.
// No symlink may be followed after the final policy check. An optional root
// confines snapshot reads; authorize supplies the invocation's path policy.
func OpenAuthorizedRegular(root, path string, authorize func(string) (string, error)) (*os.File, error) {
	if authorize == nil {
		return nil, errors.New("read policy is required")
	}
	path, err := authorize(path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if root != "" {
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("read outside root: %s", path)
		}
	}
	// Pin the admitted file identity as well: replacement by a hard link must
	// not inherit a policy decision made for the original regular file.
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	if _, err := authorize(path); err != nil {
		return nil, err
	}
	f, err := openCanonicalRegular(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err == nil && FromInfo(before) != FromInfo(after) {
		err = fmt.Errorf("file changed during authorization: %s; retry the read", path)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func openCanonicalRegular(path string) (*os.File, error) {
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	dir, err := unix.Open(string(filepath.Separator), dirFlags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = unix.Close(dir) }()
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dir, part, dirFlags, 0)
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		_ = unix.Close(dir)
		dir = next
	}
	fd, err := unix.Openat(dir, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return regularFile(os.NewFile(uintptr(fd), path), nil, path)
}
