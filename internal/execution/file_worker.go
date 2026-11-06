package execution

import (
	"context"
	"os/exec"
	"strconv"
	"strings"

	"ccdp/internal/sandbox"
)

const FileWorkerArgument = "--ccdp-internal-files-v1"

// FileWorkerProfile permits only the fixed Go file service. It cannot fork,
// load project executables or use network services, even if a shell can.
func FileWorkerProfile(policy *sandbox.Sandbox, readRoot string) (string, error) {
	if err := CheckEnvironment(policy); err != nil {
		return "", err
	}
	c := controllerFor(policy.ExecutorID)
	if c == nil {
		return "", ErrEnvironmentUnavailable
	}
	if err := c.Protect(policy); err != nil {
		return "", err
	}
	binary, err := c.Binary()
	if err != nil {
		return "", err
	}
	var profile string
	if readRoot != "" {
		profile, err = policy.ReadOnlyCommandProfileWithin(binary, readRoot)
	} else {
		profile, err = policy.Profile()
	}
	if err != nil {
		return "", err
	}
	profile = strings.ReplaceAll(profile, "(allow process-fork)\n", "")
	profile = strings.ReplaceAll(profile, "(allow process-exec)\n", "")
	profile += "(deny process-fork)\n(deny network*)\n"
	profile += "(allow process-exec (literal " + strconv.Quote(binary) + "))\n"
	profile += "(deny process-exec (require-not (literal " + strconv.Quote(binary) + ")))\n"
	return profile, nil
}

func StartFileWorker(ctx context.Context, policy *sandbox.Sandbox, profile string) (*Process, error) {
	binary, err := ExecutorBinary(policy)
	if err != nil {
		return nil, err
	}
	p := newProcess(exec.CommandContext(ctx, sandbox.BackendPath, "-p", profile, "--", binary, FileWorkerArgument))
	p.cmd.Dir, p.cmd.Env = "/", []string{"LANG=C", "GOMAXPROCS=2"}
	return bindController(p, ctx, policy), nil
}
