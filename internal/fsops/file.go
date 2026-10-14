package fsops

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Version observes identity and ordinary edits without reading file contents.
// It is a freshness check, not an atomic CAS against noncooperating writers.
type Version struct {
	Device, Inode uint64
	Size          int64
	Mtime, Ctime  time.Time
}

func FromInfo(info os.FileInfo) Version {
	v := Version{Size: info.Size(), Mtime: info.ModTime()}
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		v.Device, v.Inode = uint64(s.Dev), uint64(s.Ino)
		v.Ctime = changeTime(s)
	}
	return v
}

// OpenRegular confines resolution to root when supplied, rejects final links
// and opens nonblocking before checking the descriptor type (including FIFOs).
func OpenRegular(root, path string) (*os.File, error) {
	var f *os.File
	var err error
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if root != "" {
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		parent, e := filepath.EvalSymlinks(filepath.Dir(path))
		if e != nil {
			return nil, e
		}
		path = filepath.Join(parent, filepath.Base(path))
		r, e := os.OpenRoot(root)
		if e != nil {
			return nil, e
		}
		defer r.Close()
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return nil, e
		}
		return OpenRegularAt(r, rel)
	} else {
		f, err = os.OpenFile(path, flags, 0)
	}
	return regularFile(f, err, path)
}

func OpenRegularAt(root *os.Root, path string) (*os.File, error) {
	f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	return regularFile(f, err, path)
}

func regularFile(f *os.File, err error, path string) (*os.File, error) {
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("not a regular file: %s", path)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func Observe(path string) (Version, error) {
	f, err := OpenRegular("", path)
	if err != nil {
		return Version{}, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return Version{}, err
	}
	return FromInfo(i), nil
}

func Read(path string, limit int64) ([]byte, Version, error) {
	f, err := OpenRegular("", path)
	if err != nil {
		return nil, Version{}, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return nil, Version{}, err
	}
	v := FromInfo(i)
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, v, err
	}
	if int64(len(b)) > limit {
		return nil, v, fmt.Errorf("file exceeds %d byte editing limit", limit)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, v, err
	}
	if FromInfo(after) != v {
		return nil, v, fmt.Errorf("file changed during read: %s", path)
	}
	return b, v, nil
}

// ObserveAt observes a regular file through a pinned filesystem root.
func ObserveAt(root *os.Root, path string) (Version, error) {
	f, err := OpenRegularAt(root, path)
	if err != nil {
		return Version{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Version{}, err
	}
	return FromInfo(info), nil
}

// ReadAt reads through the same pinned root used to publish an edit.
func ReadAt(root *os.Root, path string, limit int64) ([]byte, Version, error) {
	f, err := OpenRegularAt(root, path)
	if err != nil {
		return nil, Version{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, Version{}, err
	}
	version := FromInfo(info)
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, version, err
	}
	if int64(len(data)) > limit {
		return nil, version, fmt.Errorf("file exceeds %d byte editing limit", limit)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, version, err
	}
	if FromInfo(after) != version {
		return nil, version, fmt.Errorf("file changed during read: %s", path)
	}
	return data, version, nil
}

// PublishAt requires the caller to hold LockPath through observation and records.
// root is an already opened authorized directory, never the target's parent.
// A nil expected version means create-only.
func PublishAt(ctx context.Context, root *os.Root, path string, data []byte, mode os.FileMode, expected *Version) (Version, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Version{}, err
	}
	if expected != nil {
		info, err := root.Lstat(path)
		if err != nil {
			return Version{}, err
		}
		if !info.Mode().IsRegular() || MultipleLinks(info) {
			return Version{}, fmt.Errorf("refusing to replace linked/nonregular file %s", path)
		}
		mode = info.Mode().Perm()
	}
	check := func() error {
		if expected == nil {
			return nil
		}
		current, err := ObserveAt(root, path)
		if err != nil {
			return err
		}
		if current != *expected {
			return fmt.Errorf("file modified since observation: %s; Read again", path)
		}
		return nil
	}
	if err := publishEntry(ctx, root, path, "regular", mode, data, expected == nil, check); err != nil {
		return Version{}, err
	}
	return ObserveAt(root, path)
}

func MultipleLinks(info os.FileInfo) bool {
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && s.Nlink > 1
}
