package fsops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// PublishEntry replaces a directory entry without following a destination link.
// Root prevents a concurrently replaced ancestor from escaping the workspace.
func PublishEntry(ctx context.Context, r *os.Root, rel, kind string, mode os.FileMode, b []byte) error {
	return publishEntry(ctx, r, rel, kind, mode, b, false, nil)
}

// Shared preparation/publication for file tools and snapshot application.
func publishEntry(ctx context.Context, r *os.Root, rel, kind string, mode os.FileMode, b []byte, createOnly bool, check func() error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if !filepath.IsLocal(rel) {
		return errors.New("snapshot path must be relative")
	}
	if kind == "absent" {
		e := r.Remove(rel)
		if os.IsNotExist(e) {
			return nil
		}
		return e
	}
	if kind != "regular" && kind != "symlink" {
		return errors.New("unsupported snapshot file kind")
	}
	parent := filepath.Dir(rel)
	// Reject even in-root symlink ancestors: these would alias two manifest paths.
	parts := strings.Split(filepath.ToSlash(parent), "/")
	prefix := ""
	for _, part := range parts {
		if part == "." {
			continue
		}
		prefix = filepath.Join(prefix, part)
		fi, e := r.Lstat(prefix)
		if os.IsNotExist(e) {
			if e = r.Mkdir(prefix, 0755); e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else if !fi.IsDir() {
			return errors.New("snapshot parent is not a directory")
		}
	}
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return e
	}
	tmp := filepath.Join(parent, ".ccdp-write-"+hex.EncodeToString(nonce[:]))
	defer r.Remove(tmp)
	if kind == "symlink" {
		if e := r.Symlink(string(b), tmp); e != nil {
			return e
		}
	} else {
		out, e := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if e != nil {
			return e
		}
		_, e = out.Write(b)
		if e == nil {
			e = out.Chmod(mode)
		}
		if e == nil {
			e = out.Sync()
		}
		e = errors.Join(e, out.Close())
		if e != nil {
			return e
		}
	}
	if check != nil {
		if e := check(); e != nil {
			return e
		}
	}
	if createOnly {
		if e := r.Link(tmp, rel); e != nil {
			return e
		}
		// Unlink before recording ctime: our own cleanup changes link metadata.
		if e := r.Remove(tmp); e != nil {
			return e
		}
	} else if e := r.Rename(tmp, rel); e != nil {
		return e
	}
	d, e := r.Open(parent)
	if e != nil {
		return e
	}
	return errors.Join(d.Sync(), d.Close())
}
