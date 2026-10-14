// Package executable shares the executable identity boundary used by automatic
// read-only admission and native query adapters.
package executable

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns the canonical selected executable and whether selection used
// a path independent of the working directory. Relative PATH entries before
// the selected executable make automatic approval unsafe, even when no
// candidate exists in the admission process's current directory.
func Resolve(name, searchPath, dir string) (string, bool) {
	if strings.ContainsRune(name, filepath.Separator) {
		absolute := filepath.IsAbs(name)
		if !absolute {
			name = filepath.Join(dir, name)
		}
		return File(name), absolute
	}
	stable := true
	for _, entry := range filepath.SplitList(searchPath) {
		absolute := filepath.IsAbs(entry)
		if !absolute {
			stable = false
			entry = filepath.Join(dir, entry)
		}
		if path := File(filepath.Join(entry, name)); path != "" {
			return path, stable
		}
	}
	return "", false
}

// File requires a regular executable and resolves every symlink before trust.
func File(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return ""
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Clean(path)
}

// Trusted admits system and installed application/toolchain binaries, never a
// relative PATH selection or a symlink whose target is a project directory.
func Trusted(path string) bool {
	if path == "" {
		return false
	}
	for _, root := range []string{"/bin", "/sbin", "/usr/bin", "/usr/sbin", "/Library/Developer/CommandLineTools/usr/bin", "/usr/local/bin", "/usr/local/Cellar", "/opt/homebrew/bin", "/opt/homebrew/Cellar", "/Applications"} {
		if Within(root, path) {
			return true
		}
	}
	return false
}

func Within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// FindTrusted pins a native adapter to a trusted absolute executable. Project
// PATH candidates are skipped; the system path remains an available fallback.
func FindTrusted(name, searchPath string) string {
	for _, entry := range filepath.SplitList(searchPath + ":/usr/bin:/bin") {
		if !filepath.IsAbs(entry) {
			continue
		}
		if path := File(filepath.Join(entry, name)); Trusted(path) {
			return path
		}
	}
	return ""
}
