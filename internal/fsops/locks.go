// Package fsops contains filesystem coordination shared by tools and workflows.
package fsops

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"ccdp/internal/atomicfile"
)

// LockPath serializes cooperating CCDP mutations, independently of workspace
// lifecycle leases. Replacing a target cannot replace this lock's inode.
func LockPath(path string) (*atomicfile.Lock, error) { return lock(path, "write") }

// LockWorkspace serializes repository index/ref publication, not file edits.
func LockWorkspace(root string) (*atomicfile.Lock, error) { return lock(root, "publish") }

// LeaseWorkspace prevents concurrent lifecycle owners, independently of writes.
func LeaseWorkspace(root string) (*atomicfile.Lock, error) { return lock(root, "lease") }

// LockDelivery prevents two resumes of the same plan, without holding the
// repository publication lock while candidate checks run.
func LockDelivery(root, plan string) (*atomicfile.Lock, error) { return lock(root, "delivery-"+plan) }

func lock(root, purpose string) (*atomicfile.Lock, error) {
	canonical, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// New targets may have missing parents. Resolve the existing ancestor once.
	var tail []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(canonical)
		if resolveErr == nil {
			canonical = resolved
			break
		}
		if !os.IsNotExist(resolveErr) {
			return nil, resolveErr
		}
		tail = append(tail, filepath.Base(canonical))
		canonical = filepath.Dir(canonical)
	}
	for i := len(tail) - 1; i >= 0; i-- {
		canonical = filepath.Join(canonical, tail[i])
	}
	dir := filepath.Join(os.TempDir(), "ccdp-workspace-locks")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Hash only the path to obtain a bounded filename, never file contents.
	key := sha256.Sum256([]byte(canonical))
	return atomicfile.AcquireLock(filepath.Join(dir, hex.EncodeToString(key[:])+"-"+purpose+".lock"))
}
