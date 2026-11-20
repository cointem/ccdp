package desktop

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"ccdp/internal/atomicfile"
	"ccdp/internal/fsops"
	"ccdp/internal/skills"
)

type customPlugin struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	Skills      []string `json:"skills"`
	Digest      string   `json:"digest"`
}

var localPluginID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func pluginManifest(source *os.Root) (customPlugin, error) {
	const manifestPath = ".codex-plugin/plugin.json"
	data, _, err := fsops.ReadAt(source, manifestPath, skills.DefaultMaxSkillFileBytes)
	if err != nil {
		return customPlugin{}, fmt.Errorf("无法读取 %s：%w", manifestPath, err)
	}
	var manifest struct {
		Name, Version, Description, Skills string
		Interface                          struct {
			DisplayName, ShortDescription string
		}
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return customPlugin{}, fmt.Errorf("插件清单无效：%w", err)
	}
	if !localPluginID.MatchString(manifest.Name) {
		return customPlugin{}, errors.New("插件清单需要有效的 name 标识")
	}
	if manifest.Skills != "" && filepath.Clean(manifest.Skills) != "skills" {
		return customPlugin{}, errors.New("当前支持将技能放在 skills/ 目录的本地插件")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return customPlugin{}, err
	}
	for _, key := range []string{"apps", "mcpServers", "hooks", "agents", "commands", "lspServers"} {
		if value, ok := fields[key]; ok && string(value) != "null" {
			return customPlugin{}, fmt.Errorf("此插件声明了 %s，当前仅支持技能型插件", key)
		}
	}
	for _, path := range []string{".mcp.json", ".app.json", "hooks", "agents", "commands"} {
		if _, err := source.Lstat(path); err == nil {
			return customPlugin{}, fmt.Errorf("插件包含 %s，当前仅支持技能型插件", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return customPlugin{}, err
		}
	}
	return customPlugin{ID: manifest.Name, Name: cmp.Or(strings.TrimSpace(manifest.Interface.DisplayName), manifest.Name),
		Description: cmp.Or(manifest.Interface.ShortDescription, manifest.Description), Version: manifest.Version}, nil
}

// Open the private plugin directory relative to the user skill root. An
// unmarked directory or a user's regular skill never becomes uninstallable.
func customPluginRoot(userSkills string, create bool) (*os.Root, error) {
	if userSkills == "" {
		return nil, errors.New("未指定用户技能目录")
	}
	parent, name := filepath.Dir(userSkills), filepath.Base(userSkills)
	if create {
		if err := os.MkdirAll(parent, 0700); err != nil {
			return nil, err
		}
	}
	base, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	if create {
		if err := base.Mkdir(name, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	root, err := openPluginDir(base, name)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if create {
		err = root.Mkdir(skills.ManagedPluginsDir, 0700)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return openPluginDir(root, skills.ManagedPluginsDir)
}

func openPluginDir(root *os.Root, path string) (*os.Root, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s 必须是普通目录", path)
	}
	return root.OpenRoot(path)
}

func readPluginReceipt(dir *os.Root, id string) (customPlugin, error) {
	data, _, err := fsops.ReadAt(dir, skills.ManagedPluginReceipt, skills.DefaultMaxSkillFileBytes)
	if err != nil {
		return customPlugin{}, err
	}
	var p customPlugin
	if err := json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	if p.ID != id || p.Name == "" || len(p.Digest) != sha256.Size*2 {
		return p, errors.New("本地插件的安装记录已损坏")
	}
	return p, nil
}

func installedCustomPlugins(userSkills string) ([]customPlugin, error) {
	root, err := customPluginRoot(userSkills, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var plugins []customPlugin
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !localPluginID.MatchString(e.Name()) {
			continue
		}
		dir, err := openPluginDir(root, e.Name())
		if err == nil {
			var p customPlugin
			p, err = readPluginReceipt(dir, e.Name())
			dir.Close()
			if err == nil {
				plugins = append(plugins, p)
			}
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%s：%w", e.Name(), err))
		}
	}
	return plugins, errors.Join(errs...)
}

func lockPlugins(root *os.Root) (*atomicfile.Lock, error) {
	f, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	return atomicfile.AcquireOpenedLock(f)
}

func installCustomPlugin(userSkills, sourcePath string) error {
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	plugin, err := pluginManifest(source)
	if err != nil {
		return err
	}
	sourceSkills, err := openPluginDir(source, "skills")
	if err != nil {
		return fmt.Errorf("无法打开插件的 skills/ 目录：%w", err)
	}
	defer sourceSkills.Close()
	root, err := customPluginRoot(userSkills, true)
	if err != nil {
		return err
	}
	defer root.Close()
	selected, err := filepath.EvalSymlinks(sourceSkills.Name())
	if err != nil {
		return err
	}
	managed, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return err
	}
	within, err := filepath.Rel(selected, managed)
	if err != nil || filepath.IsLocal(within) {
		return errors.New("插件安装目录不能位于源技能目录中")
	}
	lock, err := lockPlugins(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := root.Lstat(plugin.ID); err == nil {
		return fmt.Errorf("同名插件目录已存在：%s", plugin.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	stage := ".install-" + rand.Text()
	if err := root.Mkdir(stage, 0700); err != nil {
		return err
	}
	defer root.RemoveAll(stage)
	dir, err := root.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Mkdir("skills", 0700); err != nil {
		return err
	}
	dest, err := dir.OpenRoot("skills")
	if err != nil {
		return err
	}
	defer dest.Close()
	if err := copyLocalPackageTree(sourceSkills, dest); err != nil {
		return err
	}
	store := skills.NewStore()
	if err := store.LoadChecked(filepath.Abs, "", dest.Name()); err != nil {
		return err
	}
	entries, err := fs.ReadDir(dest.FS(), ".")
	if err != nil {
		return err
	}
	for _, skill := range store.All() {
		plugin.Skills = append(plugin.Skills, skill.Name)
	}
	var skillDirs int
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			skillDirs++
		}
	}
	if len(plugin.Skills) == 0 || len(plugin.Skills) != skillDirs {
		return errors.New("skills/ 中每个技能均需有效的 SKILL.md 且名称不可重复")
	}
	plugin.Digest, err = pluginTreeDigest(dir)
	if err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(plugin, "", "  ")
	if err != nil {
		return err
	}
	if err := dir.WriteFile(skills.ManagedPluginReceipt, receipt, 0600); err != nil {
		return err
	}
	return publishLocalPackage(root, stage, root, plugin.ID)
}

// The receipt covers paths, executable bits and content including supplemental
// resources. Verify before uninstalling so edits and new files can be kept.
func pluginTreeDigest(dir *os.Root) (string, error) {
	total := sha256.New()
	err := fs.WalkDir(dir.FS(), ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil || path == skills.ManagedPluginReceipt || path == "." {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(total, "%q %v\n", path, info.Mode())
		if info.IsDir() {
			return nil
		}
		f, err := fsops.OpenRegularAt(dir, path)
		if err != nil {
			return err
		}
		fileHash := sha256.New()
		_, err = io.Copy(fileHash, f)
		f.Close()
		total.Write(fileHash.Sum(nil))
		return err
	})
	return fmt.Sprintf("%x", total.Sum(nil)), err
}

func uninstallCustomPlugin(userSkills, id string) error {
	if !localPluginID.MatchString(id) {
		return errors.New("无效的插件标识")
	}
	root, err := customPluginRoot(userSkills, false)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := lockPlugins(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	dir, err := openPluginDir(root, id)
	if err != nil {
		return err
	}
	defer dir.Close()
	if _, err := readPluginReceipt(dir, id); err != nil {
		return err
	}
	// Detach before reading the final digest. A replaced source directory
	// must never inherit an integrity check made on the original handle.
	trash := ".remove-" + rand.Text()
	if err := publishLocalPackage(root, id, root, trash); err != nil {
		return err
	}
	if err := verifyRemovedPlugin(root, trash, id); err != nil {
		if restoreErr := publishLocalPackage(root, trash, root, id); restoreErr != nil {
			return fmt.Errorf("%w；插件文件保留在 %s（无法恢复：%v）", err, filepath.Join(root.Name(), trash), restoreErr)
		}
		return err
	}
	return root.RemoveAll(trash)
}

func verifyRemovedPlugin(root *os.Root, path, id string) error {
	dir, err := openPluginDir(root, path)
	if err != nil {
		return err
	}
	defer dir.Close()
	p, err := readPluginReceipt(dir, id)
	if err != nil {
		return err
	}
	digest, err := pluginTreeDigest(dir)
	if err == nil && digest != p.Digest {
		return errors.New("插件文件在安装后发生了改动，请先备份并移走该插件目录")
	}
	return err
}
