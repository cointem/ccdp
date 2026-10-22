//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package atomicfile

import (
	"fmt"
	"os"
)

// Lock is a placeholder on platforms without a supported advisory lock.
type Lock struct{}

// AcquireLock fails rather than pretending that a single-writer guarantee
// exists on an unsupported platform.
func AcquireLock(path string) (*Lock, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedLock, path)
}

func AcquireOpenedLock(f *os.File) (*Lock, error) {
	_ = f.Close()
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedLock, f.Name())
}

func (l *Lock) Close() error { return nil }
