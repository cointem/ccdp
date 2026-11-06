// Package runtimepath owns private execution directories and opaque identities.
package runtimepath

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
	r, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(abs), "/"), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid private runtime path")
		}
		info, statErr := r.Lstat(part)
		if os.IsNotExist(statErr) {
			if err := r.Mkdir(part, 0700); err != nil && !os.IsExist(err) {
				return "", err
			}
			info, statErr = r.Lstat(part)
		}
		if statErr != nil {
			return "", statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("private runtime component %q is not a real directory", part)
		}
		if i == len(parts)-1 {
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
				return "", fmt.Errorf("private runtime directory must be owned by this user with mode 0700")
			}
		}
		next, err := r.OpenRoot(part)
		if err != nil {
			return "", err
		}
		actual, e := next.Stat(".")
		if e != nil || !os.SameFile(info, actual) {
			next.Close()
			return "", fmt.Errorf("private runtime directory changed during preparation")
		}
		_ = r.Close()
		r = next
	}
	return abs, nil
}
