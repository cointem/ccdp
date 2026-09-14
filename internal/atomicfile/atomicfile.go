// Package atomicfile contains the small filesystem primitives used by the
// session store.  A published file is first written and synced in the
// destination directory, then renamed and followed by a directory sync.
//
// The package deliberately does not try to provide a transaction over more
// than one file.  Callers publish blobs before committing an event that refers
// to them.
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var (
	// ErrLocked is returned by Lock when another writer owns the advisory lock.
	ErrLocked = errors.New("atomicfile: lock is held")
	// ErrUnsupportedLock is returned on platforms where advisory locking is not
	// available through this package.
	ErrUnsupportedLock = errors.New("atomicfile: advisory locking is unsupported")
)

// SyncFile is injectable by callers that need to exercise sync failures.  A
// nil function uses (*os.File).Sync.
type SyncFile func(*os.File) error

// SyncDir syncs directory metadata after a rename.  It is kept as a public
// helper because a durable publication requires the parent directory sync,
// not only the temporary file sync.
func SyncDir(dir string) error {
	return syncDir(dir)
}

func syncFile(f *os.File, syncFn SyncFile) error {
	if syncFn != nil {
		return syncFn(f)
	}
	return f.Sync()
}

// WriteFile publishes data at path atomically.  The temporary file is always
// created beside path, so rename cannot cross filesystems.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	return WriteFileWithSync(path, data, perm, nil, nil)
}

// WriteFileWithSync is WriteFile with optional file and directory sync hooks.
// Hooks are useful to callers' fault-injection tests; production callers pass
// nil and get real fsync semantics.
func WriteFileWithSync(path string, data []byte, perm fs.FileMode, syncFn SyncFile, syncDirFn func(string) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("atomicfile: create parent %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("atomicfile: create temporary file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		_ = tmp.Close()
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("atomicfile: chmod temporary file for %s: %w", path, err)
	}
	if err := writeAll(tmp, data); err != nil {
		return fmt.Errorf("atomicfile: write temporary file for %s: %w", path, err)
	}
	if err := syncFile(tmp, syncFn); err != nil {
		return fmt.Errorf("atomicfile: sync temporary file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomicfile: close temporary file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomicfile: publish %s: %w", path, err)
	}
	removeTemp = false
	if syncDirFn == nil {
		syncDirFn = SyncDir
	}
	if err := syncDirFn(dir); err != nil {
		return fmt.Errorf("atomicfile: sync parent %s after publishing %s: %w", dir, path, err)
	}
	return nil
}

// WriteReader publishes a bounded stream atomically and returns the number of
// bytes copied.  maxBytes <= 0 means no caller-imposed limit.
func WriteReader(path string, r io.Reader, perm fs.FileMode, maxBytes int64) (int64, error) {
	return WriteReaderWithSync(path, r, perm, maxBytes, nil, nil)
}

// WriteReaderWithSync is WriteReader with optional sync hooks.
func WriteReaderWithSync(path string, r io.Reader, perm fs.FileMode, maxBytes int64, syncFn SyncFile, syncDirFn func(string) error) (int64, error) {
	if r == nil {
		return 0, errors.New("atomicfile: nil reader")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("atomicfile: create parent %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return 0, fmt.Errorf("atomicfile: create temporary file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		_ = tmp.Close()
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return 0, fmt.Errorf("atomicfile: chmod temporary file for %s: %w", path, err)
	}
	var src io.Reader = r
	if maxBytes > 0 {
		src = io.LimitReader(r, maxBytes+1)
	}
	n, err := io.Copy(tmp, src)
	if err != nil {
		return n, fmt.Errorf("atomicfile: write temporary file for %s: %w", path, err)
	}
	if maxBytes > 0 && n > maxBytes {
		return n, fmt.Errorf("atomicfile: %w: %d bytes exceeds limit %d", ErrTooLarge, n, maxBytes)
	}
	if err := syncFile(tmp, syncFn); err != nil {
		return n, fmt.Errorf("atomicfile: sync temporary file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return n, fmt.Errorf("atomicfile: close temporary file for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return n, fmt.Errorf("atomicfile: publish %s: %w", path, err)
	}
	removeTemp = false
	if syncDirFn == nil {
		syncDirFn = SyncDir
	}
	if err := syncDirFn(dir); err != nil {
		return n, fmt.Errorf("atomicfile: sync parent %s after publishing %s: %w", dir, path, err)
	}
	return n, nil
}

// ErrTooLarge indicates that a bounded stream exceeded its configured limit.
var ErrTooLarge = errors.New("atomicfile: content too large")

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
