package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The lock owns only this disposable directory, never the source workspace.
func ownReviewDirectory(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".owner"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	if _, err = fmt.Fprint(f, "ccdp-review-v1\n"); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func cleanAbandonedReviews() {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "ccdp-review-*"))
	for _, dir := range dirs {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		fd, err := unix.Open(filepath.Join(dir, ".owner"), unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		f := os.NewFile(uintptr(fd), "review owner")
		if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil {
			var marker [15]byte
			n, _ := f.ReadAt(marker[:], 0)
			if string(marker[:n]) == "ccdp-review-v1\n" {
				_ = os.RemoveAll(dir)
			}
		}
		f.Close()
	}
}
