//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package atomicfile

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// Lock is a process and cross-process advisory lock.  It is intentionally
// non-blocking: a second session runtime must fail explicitly rather than
// silently becoming a second state owner.
type Lock struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

// AcquireLock opens path and attempts an exclusive non-blocking flock.
func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("atomicfile: acquire lock %s: %w", path, err)
	}
	return &Lock{file: f}, nil
}

// Close releases the advisory lock and closes its descriptor.  It is
// idempotent so the store can use it from all error and shutdown paths.
func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	errUnlock := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	errClose := l.file.Close()
	if errUnlock != nil {
		return errUnlock
	}
	return errClose
}
