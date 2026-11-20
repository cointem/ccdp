// Package runtimepath owns private execution directories and opaque identities.
package runtimepath

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"ccdp/internal/fsops"
	"golang.org/x/sys/unix"
)

func ID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ccdp", "runtime")
}

// TemporaryRoot keeps socket paths short even with a long HOME or TMPDIR.
// EnsurePrivate must verify this user-owned boundary before creating children.
func TemporaryRoot() string {
	return filepath.Join("/tmp", fmt.Sprintf("ccdp-%d", os.Getuid()))
}

// EnsurePrivate creates components without following symlinks and verifies the
// owned boundary before any worker can receive access to it.
func EnsurePrivate(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("private runtime directory is unavailable")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// macOS exposes these system directories through fixed aliases. Resolve
	// them before walking owned components without accepting arbitrary links.
	if runtime.GOOS == "darwin" && (strings.HasPrefix(abs, "/var/") || strings.HasPrefix(abs, "/tmp/")) {
		abs = "/private" + abs
	}
	dir, err := fsops.OpenDirectoryAt(unix.AT_FDCWD, string(filepath.Separator))
	if err != nil {
		return "", &os.PathError{Op: "open private runtime", Path: abs, Err: err}
	}
	defer func() { _ = unix.Close(dir) }()
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(abs), "/"), "/")
	current := string(filepath.Separator)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid private runtime path")
		}
		current = filepath.Join(current, part)
		next, err := fsops.OpenDirectoryAt(dir, part)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(dir, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return "", &os.PathError{Op: "mkdir private runtime", Path: current, Err: err}
			}
			next, err = fsops.OpenDirectoryAt(dir, part)
		}
		if err != nil {
			return "", &os.PathError{Op: "open private runtime", Path: current, Err: err}
		}
		_ = unix.Close(dir)
		dir = next
	}
	// O_DIRECTORY|O_NOFOLLOW checked each component atomically. Verify the
	// owned boundary on its pinned descriptor, not a path that could be swapped
	// between lstat and open.
	var st unix.Stat_t
	if err := unix.Fstat(dir, &st); err != nil {
		return "", &os.PathError{Op: "stat private runtime", Path: abs, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != os.Getuid() || st.Mode&0077 != 0 {
		return "", fmt.Errorf("private runtime directory %q must be owned by this user with mode 0700", abs)
	}
	return abs, nil
}
