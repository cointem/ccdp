package desktop

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"ccdp/internal/atomicfile"
	"ccdp/internal/config"
	"ccdp/internal/fsops"
	"ccdp/internal/mcp"
	"ccdp/internal/skills"
)

func loadCustomizationExtensions(workspace, configPath string) *projectExtensions {
	user, configErr := config.LoadFrom(configPath)
	rows, userErr := readSkillScope(&user, userSkillDirectory(), config.SourceUserFile)
	result, effective := configuredExtensions(workspace, user, configErr, config.DefaultTrustStore(), true)
	_, hasExa := user.MCPServers[mcp.ExaServer]
	result.restoreExa = configErr == nil && !hasExa
	var projectErr error
	if workspace != "" && result.problem == "" {
		dir := filepath.Join(config.ProjectSettingsDir(effective.Workspace), "skills")
		var projectRows []projectExtension
		projectRows, projectErr = readSkillScope(&effective, dir, config.SourceProject)
		rows = append(rows, projectRows...)
	}
	slices.SortStableFunc(rows, func(a, b projectExtension) int { return strings.Compare(a.name, b.name) })
	result.skills = rows
	if err := errors.Join(userErr, projectErr); err != nil {
		result.skillsProblem = err.Error()
	}
	return result
}

func readSkillScope(cfg *config.Config, dir string, source config.Source) ([]projectExtension, error) {
	store := skills.NewStore()
	userDir := dir
	var projects []string
	if source == config.SourceProject {
		userDir, projects = "", []string{dir}
	}
	if err := store.LoadChecked(cfg.Sandbox().ResolveRead, userDir, projects...); err != nil {
		return nil, err
	}
	var rows []projectExtension
	for _, item := range store.All() {
		rows = append(rows, projectExtension{name: item.Name, detail: item.Description, source: source, path: item.CanonicalFile})
	}
	return rows, nil
}

// copyLocalPackageTree copies packaged scripts and resources along with their
// read/execute bits. Only entry types that can be carried across unchanged are
// accepted. Input and output stay anchored to the selected directory handles.
func copyLocalPackageTree(src, dst *os.Root) error {
	return fs.WalkDir(src.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		if entry.IsDir() {
			return dst.Mkdir(path, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("包内只支持普通文件和目录：%s", path)
		}
		input, err := fsops.OpenRegularAt(src, path)
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return err
		}
		output, err := dst.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()&0755|0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(output, input)
		return errors.Join(err, output.Close())
	})
}

// Publish a fully validated directory without replacing a concurrent install
// (including an existing empty directory). Both names must be single entries.
func publishLocalPackage(staged *os.Root, stagedName string, destination *os.Root, finalName string) error {
	for _, name := range []string{stagedName, finalName} {
		if !filepath.IsLocal(name) || filepath.Base(name) != name || name == "." {
			return fmt.Errorf("无效的包目录名称：%q", name)
		}
	}
	from, err := staged.Open(".")
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := destination.Open(".")
	if err != nil {
		return err
	}
	defer to.Close()
	return renameLocalPackage(int(from.Fd()), stagedName, int(to.Fd()), finalName)
}

func installLocalSkill(sourceDir, workspace string, projectScope bool) error {
	scopeRoot, err := os.UserHomeDir()
	if projectScope {
		scopeRoot, err = config.NormalizeProjectRoot(workspace)
	}
	if err != nil {
		return err
	}
	source, err := config.NormalizeProjectRoot(sourceDir)
	if err != nil {
		return err
	}
	name := filepath.Base(source)
	if !filepath.IsLocal(name) || strings.HasPrefix(name, ".") {
		return errors.New("请选择包含 SKILL.md 的技能目录")
	}
	src, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, _, err := fsops.ReadAt(src, "SKILL.md", skills.DefaultMaxSkillFileBytes); err != nil {
		return fmt.Errorf("无法读取技能的 SKILL.md：%w", err)
	}
	base, err := os.OpenRoot(scopeRoot)
	if err != nil {
		return err
	}
	defer base.Close()
	rel := filepath.Join(".ccdp", "skills")
	if err := base.MkdirAll(rel, 0700); err != nil {
		return err
	}
	dest, err := base.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer dest.Close()
	destPath, err := filepath.EvalSymlinks(dest.Name())
	if err != nil {
		return err
	}
	if within, err := filepath.Rel(source, destPath); err != nil || filepath.IsLocal(within) {
		return errors.New("请选择单个技能目录，安装位置不能位于源目录中")
	}
	if _, err := dest.Lstat(name); err == nil {
		return fmt.Errorf("技能目录 %q 已存在", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The runtime skips dot directories. Preserve the final directory name
	// inside staging because skills without a frontmatter name inherit it.
	stagingName := ".install-" + rand.Text()
	if err := dest.Mkdir(stagingName, 0700); err != nil {
		return err
	}
	defer dest.RemoveAll(stagingName)
	stage, err := dest.OpenRoot(stagingName)
	if err != nil {
		return err
	}
	defer stage.Close()
	if err := stage.Mkdir(name, 0700); err != nil {
		return err
	}
	pkg, err := stage.OpenRoot(name)
	if err != nil {
		return err
	}
	defer pkg.Close()
	if err := copyLocalPackageTree(src, pkg); err != nil {
		return err
	}
	readWithin := func(path string) (string, error) {
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(destPath, canonical)
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("技能文件位于安装目录外：%s", path)
		}
		return canonical, nil
	}
	pending := skills.NewStore()
	if err := pending.LoadChecked(readWithin, stage.Name()); err != nil {
		return err
	}
	stagedSkills := pending.All()
	if len(stagedSkills) != 1 {
		return errors.New("SKILL.md 需要包含技能说明正文")
	}
	// Lock the registry directory while checking effective names: two source
	// folders may declare the same frontmatter name.
	dir, err := dest.Open(".")
	if err != nil {
		return err
	}
	lock, err := atomicfile.AcquireOpenedLock(dir)
	if errors.Is(err, atomicfile.ErrLocked) {
		return errors.New("其他窗口正在安装技能，请重试")
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	current := skills.NewStore()
	if projectScope {
		err = current.LoadChecked(readWithin, "", dest.Name())
	} else {
		err = current.LoadChecked(readWithin, dest.Name())
	}
	if err != nil {
		return err
	}
	if _, exists := current.Get(stagedSkills[0].Name); exists {
		return fmt.Errorf("此范围已存在同名技能 %q", stagedSkills[0].Name)
	}
	err = publishLocalPackage(stage, name, dest, name)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("技能目录 %q 已存在", name)
	}
	return err
}
