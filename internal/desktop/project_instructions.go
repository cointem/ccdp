package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"ccdp/internal/fsops"
	"ccdp/internal/workspace"
)

// A project editor owns only the two instruction files in the selected workspace.
// Use the runtime's rooted reads, write locks, size limit and versioned publication.
type projectInstruction struct {
	name, text, original string
	version              *fsops.Version
	readError            error
}

func readProjectInstruction(path, name string) projectInstruction {
	doc := projectInstruction{name: name}
	root, err := os.OpenRoot(path)
	if err != nil {
		doc.readError = err
		return doc
	}
	defer root.Close()
	data, version, err := fsops.ReadAt(root, name, workspace.DefaultMaxInstructionFileBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A missing instruction file is created only if the user saves text.
	case err != nil:
		doc.readError = err
	case !utf8.Valid(data):
		doc.readError = errors.New("文件不是有效的 UTF-8 文本")
	default:
		doc.text, doc.original, doc.version = string(data), string(data), &version
	}
	return doc
}

func (d *projectInstruction) save(ctx context.Context, path string) error {
	if d.readError != nil {
		return d.readError
	}
	if d.original == d.text {
		return nil
	}
	if len(d.text) > workspace.DefaultMaxInstructionFileBytes {
		return fmt.Errorf("项目指令超过 %d KiB 上限", workspace.DefaultMaxInstructionFileBytes/1024)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := fsops.LockPath(filepath.Join(path, d.name))
	if err != nil {
		return err
	}
	defer lock.Close()
	info, err := root.Lstat(d.name)
	switch {
	case errors.Is(err, os.ErrNotExist) && d.version == nil:
	case errors.Is(err, os.ErrNotExist):
		return errors.New("文件已被删除，请重新读取")
	case err != nil:
		return err
	case d.version == nil || fsops.FromInfo(info) != *d.version:
		return errors.New("文件已在其他位置修改，请重新读取")
	case info.Mode().Perm()&0222 == 0:
		return errors.New("文件为只读")
	}
	version, err := fsops.PublishAt(ctx, root, d.name, []byte(d.text), 0644, d.version)
	if err == nil {
		d.version, d.original = &version, d.text
	}
	return err
}
