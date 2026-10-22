package execution

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"ccdp/internal/sandbox"
)

// CodeWorker uses a separate deny-by-default profile, never the workspace
// profile (which deliberately allows host reads). There is no shell, network,
// project/session read, filesystem write, or process-fork permission.
func CodeWorker(ctx context.Context, executable string, args ...string) (*Process, error) {
	var err error
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	profile, err := CodeWorkerProfile(executable)
	if err != nil {
		return nil, err
	}
	p := newProcess(exec.CommandContext(ctx, sandbox.BackendPath, append([]string{"-p", profile, "--", executable}, args...)...))
	p.cmd.Dir = "/"
	p.cmd.Env = []string{"LANG=C", "TZ=UTC", "GOMAXPROCS=2"}
	return p, nil
}

func CodeWorkerProfile(executable string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("Code Mode requires the Darwin Seatbelt backend")
	}
	path, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("worker executable must be absolute")
	}
	q := strconv.Quote(path)
	return `(version 1)
(deny default)
(import "dyld-support.sb")
; Go needs BSD and Mach calls for threads, VM, signals and pipe I/O.
; This permits syscall entry, not the separately checked file/network/process
; operations: process-fork stays denied and process-exec is limited below.
(allow syscall*)
(allow mach-bootstrap)
(allow process-exec (literal ` + q + `))
(allow file-read* (literal ` + q + `))
(allow file-map-executable (literal ` + q + `) (subpath "/usr/lib") (subpath "/System/Library"))
(allow file-read-metadata file-test-existence (path-ancestors ` + q + `))
(allow file-read* (subpath "/usr/lib") (subpath "/System/Library"))
(allow file-read-metadata (literal "/") (literal "/dev") (literal "/private"))
(allow file-read* file-write* (literal "/dev/null"))
(allow sysctl-read)
(allow mach-lookup (global-name "com.apple.system.logger"))
(allow signal (target self))
`, nil
}
