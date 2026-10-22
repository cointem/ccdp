// Package fsops contains filesystem coordination shared by tools and workflows.
package fsops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	if purpose == "" || purpose == "." || purpose == ".." || strings.ContainsAny(purpose, "/\\\x00") {
		return nil, fmt.Errorf("invalid lock purpose: %q", purpose)
	}
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
	projectPath, err := filepath.Rel(string(filepath.Separator), canonical)
	if err != nil || !filepath.IsLocal(projectPath) {
		return nil, fmt.Errorf("invalid lock path: %s", canonical)
	}
	dir := filepath.Join(os.TempDir(), "ccdp-workspace-locks", purpose, projectPath)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Lock the mirrored directory itself. No reserved filename can collide
	// with a child path, and replacing the target does not replace this inode.
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	return atomicfile.AcquireOpenedLock(f)
}
