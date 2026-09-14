//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || windows)

package atomicfile

import "errors"

func syncDir(string) error {
	return errors.New("atomicfile: directory sync is unsupported on this platform")
}
