package desktop

import "golang.org/x/sys/unix"

func renameLocalPackage(from int, staged string, to int, name string) error {
	return unix.RenameatxNp(from, staged, to, name, unix.RENAME_EXCL)
}
