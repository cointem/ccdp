// Command ccdp-desktop runs the native MyGo client of ccdp.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"ccdp/internal/codemode"
	"ccdp/internal/config"
	"ccdp/internal/desktop"
	"ccdp/internal/executable"
)

func main() {
	// Code mode starts workers by re-executing this binary. Dispatch before
	// initializing MyGo so a tool worker never opens another desktop window.
	if len(os.Args) == 2 && os.Args[1] == codemode.WorkerArgument {
		os.Exit(codemode.WorkerMain(os.Stdin, os.Stdout))
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "ccdp-desktop currently supports macOS")
		os.Exit(1)
	}
	if err := enableCrashLog(); err != nil {
		fmt.Fprintln(os.Stderr, "ccdp-desktop: cannot open crash log:", err)
	}
	dir := flag.String("d", "", "project directory")
	resume := flag.String("r", "", "resume a saved session")
	configFile := flag.String("config-file", config.ConfigPath(), "configuration file")
	flag.Parse()
	if err := executable.UseDesktopBundle(); err != nil {
		fail(err)
	}
	cfg, err := config.LoadFrom(*configFile)
	if err != nil {
		fail(err)
	}
	if *dir != "" {
		if err := cfg.ApplyCLIOverride("workspace", *dir); err != nil {
			fail(err)
		}
	} else if cfg.Workspace == "" || cfg.Workspace == "." {
		cwd, _ := os.Getwd()
		// Finder launches normally start in /; ask for a project there.
		if cwd != "/" && !strings.Contains(cwd, ".app/Contents/") {
			cfg.Workspace = cwd
		} else {
			cfg.Workspace = ""
		}
	}
	if cfg.Workspace != "" {
		cfg.Workspace, err = filepath.Abs(cfg.Workspace)
		if err != nil {
			fail(err)
		}
	}
	if err := desktop.Run(cfg, *configFile, *resume); err != nil {
		fail(err)
	}
}

// Finder launches have no terminal to retain the stack of an unhandled Go panic.
func enableCrashLog() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, "Library", "Logs", "ccdp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, "crash.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	return debug.SetCrashOutput(file, debug.CrashOptions{})
}

func fail(err error) { fmt.Fprintln(os.Stderr, "ccdp-desktop:", err); os.Exit(1) }
