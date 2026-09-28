package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"ccdp/internal/atomicfile"
)

func validateDeliveryCandidate(dir string) error {
	if filepath.Clean(filepath.Dir(dir)) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(dir), "ccdp-delivery-") {
		return errors.New("unowned delivery candidate")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("delivery candidate is not a directory")
	}
	return nil
}

func removeDeliveryCandidate(dir string) error {
	if dir == "" {
		return nil
	}
	if err := validateDeliveryCandidate(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(dir)
}

// The extra hard link proves ownership without putting metadata in Git's
// binary index.lock. Callers must hold changes.LockWorkspace throughout.
func deliveryIndexOwnerPath(index, id string) string {
	return index + ".ccdp-" + hashBytes([]byte(id)) + ".lock"
}

func recoverDeliveryIndexLock(index, id string) error {
	owner := deliveryIndexOwnerPath(index, id)
	owned, err := os.Lstat(owner)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !owned.Mode().IsRegular() {
		return errors.New("delivery index owner is not a regular file")
	}
	locked, err := os.Lstat(index + ".lock")
	if err == nil {
		if !locked.Mode().IsRegular() || !os.SameFile(owned, locked) {
			return errors.New("index lock belongs to another Git operation; not removed")
		}
		if err = os.Remove(index + ".lock"); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.Remove(owner); err != nil {
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(index))
}

func createDeliveryIndexLock(index, id string) (*os.File, error) {
	owner := deliveryIndexOwnerPath(index, id)
	f, err := os.OpenFile(owner, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = os.Link(owner, index+".lock"); err != nil {
		f.Close()
		_ = os.Remove(owner)
		return nil, err
	}
	if err = atomicfile.SyncDir(filepath.Dir(index)); err != nil {
		f.Close()
		_ = os.Remove(index + ".lock")
		_ = os.Remove(owner)
		return nil, fmt.Errorf("persist index lock ownership: %w", err)
	}
	return f, nil
}
