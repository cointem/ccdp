package executable

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Set by the desktop entry point before creating any session or tool worker.
var desktopBundle string

func DesktopBundle() string { return desktopBundle }

// UseDesktopBundle gives native queries and shell commands in a packaged
// desktop session access to the helper shipped with that particular app.
func UseDesktopBundle() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	macos := filepath.Dir(self)
	contents := filepath.Dir(macos)
	bundle := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || filepath.Ext(bundle) != ".app" {
		return nil // Running from source.
	}
	desktopBundle = bundle
	bin := filepath.Join(contents, "Resources", "bin")
	if bundledRipgrep() == "" {
		return nil // An incomplete bundle can still use a trusted system installation.
	}
	// A POSIX PATH cannot represent a directory containing a colon. The
	// native Rg tool still uses its absolute path in a bundle with that name.
	if !strings.ContainsRune(bin, os.PathListSeparator) {
		path := os.Getenv("PATH")
		if path == "" {
			path = "/usr/bin:/bin:/usr/sbin:/sbin"
		}
		if err := os.Setenv("PATH", path+string(os.PathListSeparator)+bin); err != nil {
			return err
		}
	}
	return nil
}

func bundledRipgrep() string {
	if desktopBundle == "" {
		return ""
	}
	path := filepath.Join(desktopBundle, "Contents", "Resources", "bin", "rg")
	if File(path) != path {
		return ""
	}
	return path
}
