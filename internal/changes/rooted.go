package changes

import (
	"ccdp/internal/atomicfile"
	"ccdp/internal/fsops"
	"context"
	"os"
)

// LockWorkspace coordinates repository publishers, not ordinary file edits.
func LockWorkspace(root string) (*atomicfile.Lock, error) { return fsops.LockWorkspace(root) }

func writeRooted(ctx context.Context, r *os.Root, rel string, f File, b []byte) error {
	if _, err := safePath(r.Name(), rel); err != nil {
		return err
	}
	return fsops.PublishEntry(ctx, r, rel, f.Kind, os.FileMode(f.Mode), b)
}
