package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// GitControlPaths returns the Git metadata entries which a local process
// should not be able to change for the repository containing workspace. It
// intentionally leaves the index, objects, refs, and ordinary worktree files
// writable so normal Git commands can continue to work. Discovery reads the
// worktree marker and config files directly; it does not launch Git while
// constructing a policy.
func GitControlPaths(workspace string) []string {
	if strings.TrimSpace(workspace) == "" {
		return nil
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil
	}
	workspace = filepath.Clean(workspace)
	root, gitDir, commonDir, marker, linked, ok := findGitMetadata(workspace)
	if !ok {
		return nil
	}

	paths := make([]string, 0, 24)
	add := func(path string) {
		if path == "" {
			return
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return
		}
		path = filepath.Clean(path)
		for _, existing := range paths {
			if existing == path {
				return
			}
		}
		paths = append(paths, path)
	}
	addWithLock := func(path string) {
		add(path)
		add(path + ".lock")
	}

	// The repository config is shared by linked worktrees. Worktree config is
	// stored beside that worktree's gitdir and may be created later if enabled.
	addWithLock(filepath.Join(commonDir, "config"))
	addWithLock(filepath.Join(gitDir, "config.worktree"))
	add(filepath.Join(commonDir, "hooks"))
	add(filepath.Join(commonDir, "hooks.lock"))

	// core.hooksPath may redirect executable hook files into another writable
	// root. Parse the repository's config and its includes without running Git.
	for _, hooksPath := range gitHooksPaths(gitDir, commonDir) {
		if filepath.IsAbs(hooksPath) {
			add(hooksPath)
			add(hooksPath + ".lock")
		} else {
			add(filepath.Join(root, hooksPath))
			add(filepath.Join(root, hooksPath) + ".lock")
			if workspace != root {
				add(filepath.Join(workspace, hooksPath))
				add(filepath.Join(workspace, hooksPath) + ".lock")
			}
		}
	}

	// A linked worktree's .git file selects its real metadata directory. The
	// gitdir and commondir files in that directory point back to the worktree
	// and shared repository; protect those pointer files and atomic lock names.
	if linked {
		addWithLock(marker)
	}
	for _, name := range []string{"gitdir", "commondir"} {
		path := filepath.Join(gitDir, name)
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			addWithLock(path)
		}
	}
	return paths
}

// GitControlEntryPaths returns Git selector entries that need unlink-only
// protection. A symlinked .git marker resolves to the metadata directory, so
// protecting it as a directory would also block the index and object writes
// required by ordinary Git commands.
func GitControlEntryPaths(workspace string) []string {
	if strings.TrimSpace(workspace) == "" {
		return nil
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil
	}
	_, _, _, marker, _, ok := findGitMetadata(filepath.Clean(workspace))
	if !ok || marker == "" {
		return nil
	}
	info, err := os.Lstat(marker)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	return []string{marker}
}

func findGitMetadata(workspace string) (root, gitDir, commonDir, marker string, linked, ok bool) {
	for current := workspace; ; current = filepath.Dir(current) {
		candidate := filepath.Join(current, ".git")
		info, err := os.Lstat(candidate)
		if err == nil {
			if !info.IsDir() && info.Mode()&os.ModeSymlink != 0 {
				info, err = os.Stat(candidate)
			}
			if err == nil && info.IsDir() {
				gitDir = resolveGitPath(candidate)
				return current, gitDir, gitDir, candidate, false, gitDir != ""
			}
			if err == nil && info.Mode().IsRegular() {
				gitDir, ok = readGitDirFile(candidate, current)
				if !ok {
					return "", "", "", "", false, false
				}
				commonDir = readGitCommonDir(gitDir)
				if commonDir == "" {
					return "", "", "", "", false, false
				}
				return current, gitDir, commonDir, candidate, true, true
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}

	// Also handle a bare repository selected directly as the workspace.
	if isBareGitDirectory(workspace) {
		return workspace, resolveGitPath(workspace), resolveGitPath(workspace), "", false, true
	}
	return "", "", "", "", false, false
}

func readGitDirFile(marker, root string) (string, bool) {
	data, err := os.ReadFile(marker)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	value, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(root, value)
	}
	return resolveGitPath(value), true
}

func readGitCommonDir(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return resolveGitPath(gitDir)
	}
	value := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if value == "" {
		return ""
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(gitDir, value)
	}
	return resolveGitPath(value)
}

func resolveGitPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return abs
}

func isBareGitDirectory(path string) bool {
	if info, err := os.Stat(filepath.Join(path, "HEAD")); err != nil || !info.Mode().IsRegular() {
		return false
	}
	for _, name := range []string{"config", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(path, name)); err != nil {
			return false
		}
	}
	return true
}

func gitHooksPaths(gitDir, commonDir string) []string {
	var paths []string
	visited := map[string]bool{}
	for _, path := range []string{
		filepath.Join(commonDir, "config"),
		filepath.Join(gitDir, "config"),
		filepath.Join(gitDir, "config.worktree"),
		gitSystemConfigPath(),
		gitGlobalConfigPath(),
	} {
		readGitConfig(path, visited, &paths, 0)
	}
	for key, value := range gitConfigEnvironment() {
		if strings.EqualFold(key, "core.hooksPath") && value != "" {
			paths = append(paths, expandGitPath(value))
		}
	}
	return paths
}

func gitSystemConfigPath() string {
	if os.Getenv("GIT_CONFIG_NOSYSTEM") != "" {
		return ""
	}
	if path := os.Getenv("GIT_CONFIG_SYSTEM"); path != "" {
		return path
	}
	return "/etc/gitconfig"
}

func gitGlobalConfigPath() string {
	if path := os.Getenv("GIT_CONFIG_GLOBAL"); path != "" {
		return path
	}
	// Child processes receive a per-invocation HOME, so the host user's default
	// ~/.gitconfig is not active unless GIT_CONFIG_GLOBAL explicitly selects it.
	return ""
}

func gitConfigEnvironment() map[string]string {
	values := map[string]string{}
	count, err := strconv.Atoi(os.Getenv("GIT_CONFIG_COUNT"))
	if err != nil || count < 0 || count > 256 {
		return values
	}
	for i := 0; i < count; i++ {
		key := os.Getenv("GIT_CONFIG_KEY_" + strconv.Itoa(i))
		value := os.Getenv("GIT_CONFIG_VALUE_" + strconv.Itoa(i))
		if key != "" {
			values[key] = value
		}
	}
	return values
}

func readGitConfig(path string, visited map[string]bool, hooks *[]string, depth int) {
	if path == "" || depth > 16 {
		return
	}
	path = expandGitPath(path)
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if visited[path] {
		return
	}
	visited[path] = true
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	section := ""
	var includes []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end <= 1 {
				section = ""
				continue
			}
			fields := strings.Fields(line[1:end])
			if len(fields) == 0 {
				section = ""
			} else {
				section = strings.ToLower(fields[0])
			}
			continue
		}
		key, value, ok := parseGitConfigLine(line)
		if !ok {
			continue
		}
		switch {
		case section == "core" && strings.EqualFold(key, "hooksPath"):
			if value = expandGitPath(value); value != "" {
				*hooks = append(*hooks, value)
			}
		case (section == "include" || section == "includeif") && strings.EqualFold(key, "path") && value != "":
			include := expandGitPath(value)
			if !filepath.IsAbs(include) {
				include = filepath.Join(filepath.Dir(path), include)
			}
			includes = append(includes, include)
		}
	}
	for _, include := range includes {
		readGitConfig(include, visited, hooks, depth+1)
	}
}

func parseGitConfigLine(line string) (key, value string, ok bool) {
	quote := false
	escaped := false
	sep := -1
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote {
			escaped = true
			continue
		}
		if r == '"' {
			quote = !quote
			continue
		}
		if !quote && (r == '=' || r == ' ' || r == '\t') {
			sep = i
			break
		}
	}
	if sep < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:sep])
	rest := strings.TrimSpace(line[sep:])
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimSpace(rest[1:])
	}
	if key == "" {
		return "", "", false
	}
	value = gitConfigValue(rest)
	return key, value, true
}

func gitConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, `"`) {
		if decoded, err := strconv.Unquote(value); err == nil {
			return decoded
		}
		return ""
	}
	for i, r := range value {
		if (r == '#' || r == ';') && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
			value = strings.TrimSpace(value[:i])
			break
		}
	}
	return value
}

func expandGitPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "~" {
		if home := os.Getenv("HOME"); home != "" {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home := os.Getenv("HOME"); home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
