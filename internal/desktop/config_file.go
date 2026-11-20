package desktop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/atomicfile"
	"ccdp/internal/config"
	"ccdp/internal/fsops"
)

// Keep the raw document so an edit preserves formatting and fields the
// current runtime does not interpret. Text is also our external-edit baseline.
type configFileSnapshot struct {
	Text   string
	target string
	exists bool
	mode   fs.FileMode
}

func readConfigFile(path string) (configFileSnapshot, error) {
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A dangling file symlink is an existing entry. Replacing it would
		// silently disconnect the user's dotfiles from the link target.
		if _, statErr := os.Lstat(path); errors.Is(statErr, fs.ErrNotExist) {
			return configFileSnapshot{Text: "{}\n", target: path, mode: 0600}, nil
		}
	}
	if err != nil {
		return configFileSnapshot{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return configFileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return configFileSnapshot{}, fmt.Errorf("配置路径不是普通文件：%s", path)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return configFileSnapshot{}, err
	}
	return configFileSnapshot{Text: string(data), target: target, exists: true, mode: info.Mode().Perm()}, nil
}

func saveConfigFile(path string, original configFileSnapshot, edited string) (config.Config, error) {
	if !strings.HasPrefix(strings.TrimSpace(edited), "{") {
		return config.Config{}, errors.New("配置必须是 JSON 对象")
	}
	data := []byte(edited)
	resolved, err := config.ParseFile(path, data)
	if err != nil {
		return config.Config{}, err
	}
	lock, err := fsops.LockPath(original.target)
	if err != nil {
		return config.Config{}, err
	}
	defer lock.Close()
	current, err := readConfigFile(path)
	if err != nil {
		return config.Config{}, err
	}
	if current != original {
		return config.Config{}, errors.New("配置文件已在其他位置修改，请重新打开配置后再保存")
	}
	if current.exists && current.Text == edited {
		return resolved, nil
	}
	if current.exists && current.mode&0222 == 0 {
		return config.Config{}, fmt.Errorf("配置文件为只读：%s", path)
	}
	mode := current.mode
	if resolved.ExaAPIKey != "" {
		mode &= 0600
	}
	for _, server := range resolved.MCPServers {
		if len(server.Env) > 0 {
			mode &= 0600
			break
		}
	}
	return resolved, atomicfile.WriteFile(current.target, data, mode)
}
