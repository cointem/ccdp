//go:build windows

package atomicfile

import "errors"

func syncDir(string) error {
	return errors.New("atomicfile: directory sync is unsupported on windows")
}
