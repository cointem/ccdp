package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/atomicfile"
	"ccdp/internal/config"
)

type projectMetadata struct {
	Name      string `json:"name,omitempty"`
	Pinned    bool   `json:"pinned,omitempty"`
	Collapsed bool   `json:"collapsed,omitempty"`
}

// Items is the main window's snapshot. The presence of a key registers a
// project even when it has no sessions or customized metadata.
type projectStore struct {
	path  string
	Items map[string]projectMetadata
}

func projectStorePath() string {
	return filepath.Join(filepath.Dir(config.ConfigPath()), "desktop-projects.json")
}

func projectKey(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	// Unmounted and moved folders may still have history in the sidebar.
	return abs
}

func loadProjectStore(path string) (*projectStore, error) {
	s := &projectStore{path: path, Items: make(map[string]projectMetadata)}
	items, err := s.read()
	if err == nil {
		s.Items = items
	}
	return s, err
}

func (s *projectStore) read() (map[string]projectMetadata, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return make(map[string]projectMetadata), nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取项目列表：%w", err)
	}
	var items map[string]projectMetadata
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("读取项目列表：%w", err)
	}
	if items == nil {
		return nil, errors.New("无法读取项目列表：需要项目路径和设置的映射")
	}
	for key := range items {
		if !filepath.IsAbs(key) || filepath.Clean(key) != key {
			return nil, fmt.Errorf("项目列表中的路径无效：%q", key)
		}
	}
	return items, nil
}

func (s *projectStore) get(path string) projectMetadata {
	if s == nil {
		return projectMetadata{}
	}
	if item, ok := s.Items[path]; ok {
		return item
	}
	return s.Items[projectKey(path)]
}

// update changes only the supplied fields on the latest on-disk project.
// Passing nil registers the folder without changing existing preferences.
func (s *projectStore) update(path string, edit func(*projectMetadata)) error {
	if s == nil || s.path == "" {
		return errors.New("项目列表不可用")
	}
	key := projectKey(path)
	if key == "" {
		return errors.New("请选择项目文件夹")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("创建项目列表目录：%w", err)
	}
	lock, err := atomicfile.AcquireLock(s.path + ".lock")
	if errors.Is(err, atomicfile.ErrLocked) {
		return fmt.Errorf("另一个窗口正在更新项目列表，请重试：%w", err)
	}
	if err != nil {
		return fmt.Errorf("锁定项目列表：%w", err)
	}
	defer lock.Close()
	items, err := s.read()
	if err != nil {
		return err
	}
	before, exists := items[key]
	after := before
	if edit != nil {
		edit(&after)
		after.Name = strings.TrimSpace(after.Name)
	}
	if exists && before == after {
		s.Items = items
		return nil
	}
	items[key] = after
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	err = atomicfile.WriteFile(s.path, append(data, '\n'), 0600)
	var published *atomicfile.PublishedError
	if err == nil || errors.As(err, &published) {
		s.Items = items
	}
	if err != nil {
		return fmt.Errorf("保存项目列表：%w", err)
	}
	return nil
}
