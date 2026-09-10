package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"ccdp/internal/sandbox"
)

// DefaultShellTimeout bounds a single Bash tool invocation.
const DefaultShellTimeout = 120 * time.Second

// waitPipeDelay bounds how long Wait may linger after the shell exits when
// grandchild processes (e.g. `npm run dev &`) inherited stdout/stderr and keep
// the pipes open. Once it elapses, exec closes the pipe ends so the scanner
// goroutine gets EOF instead of blocking forever.
const waitPipeDelay = 3 * time.Second

// setProcessGroup configures cmd to run in its own process group, so that a
// context timeout kills the whole tree (children and grandchildren) rather
// than only the shell, and bounds pipe-drain lingering with WaitDelay.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the entire process group (negative pid). ErrProcessDone tells
		// Wait to ignore a race where the group died before the kill landed.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = waitPipeDelay
}

// benignPipeClose reports whether err is the benign Wait error after WaitDelay
// force-closed pipes that grandchildren kept open. The shell itself exited
// fine — this is not a launch failure and the collected output stands.
func benignPipeClose(err error) bool {
	return errors.Is(err, exec.ErrWaitDelay) || errors.Is(err, os.ErrClosed)
}

// BashResult is the structured output of a shell invocation.
type BashResult struct {
	ExitCode int
	Output   string
	Timeout  bool
}

// RunShell executes a command through the user's login shell with a timeout.
func RunShell(ctx *Context, command string) (BashResult, error) {
	timeout := ctx.Timeout
	if timeout <= 0 {
		timeout = DefaultShellTimeout
	}

	// Resolve the shell; prefer $SHELL then fall back to /bin/sh.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	shellFlag := "-c"
	if strings.Contains(shell, "fish") {
		shellFlag = "-c" // fish uses -c too; command override is fine for v1
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, shell, shellFlag, command)
	cmd.Dir = ctx.WorkingDir
	cmd.Env = os.Environ()
	setProcessGroup(cmd)

	// Stream output lines to the Notify callback when one is set (live tool
	// output in the TUI), while still collecting the full output for the result.
	collected := &strings.Builder{}
	if ctx.Notify != nil {
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw

		scannerDone := make(chan struct{})
		go func() {
			defer close(scannerDone)
			sc := bufio.NewScanner(pr)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				line := sc.Text()
				ctx.Notify(line)
				collected.WriteString(line)
				collected.WriteString("\n")
			}
		}()

		err := cmd.Run()
		_ = pw.Close()
		<-scannerDone
		res := BashResult{Output: collected.String()}
		if cctx.Err() == context.DeadlineExceeded {
			res.Timeout = true
			res.ExitCode = -1
			res.Output += fmt.Sprintf("\n[command timed out after %s]", timeout)
			return res, nil
		}
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
			} else if benignPipeClose(err) {
				// Grandchildren held the pipes; WaitDelay force-closed them
				// after the shell exited. Not a bash failure.
			} else {
				return res, fmt.Errorf("bash: %w", err)
			}
		}
		return res, nil
	}

	out := &strings.Builder{}
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	res := BashResult{Output: out.String()}
	if cctx.Err() == context.DeadlineExceeded {
		res.Timeout = true
		res.ExitCode = -1
		res.Output += fmt.Sprintf("\n[command timed out after %s]", timeout)
		return res, nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else if benignPipeClose(err) {
			// See above: leftover grandchildren holding the pipes.
		} else {
			return res, fmt.Errorf("bash: %w", err)
		}
	}
	return res, nil
}

// BashTool runs shell commands. It is the agent's main ability to interact
// with the real environment.
type BashTool struct{}

// NewBashTool creates the bash tool.
func NewBashTool() *BashTool { return &BashTool{} }

func (b *BashTool) Name() string { return "Bash" }

func (b *BashTool) Description() string {
	return `Run a bash command. Use this tool to execute shell commands, scripts,
tests, build steps and inspect the environment. Returns stdout and stderr,
followed by the exit code. Long-running commands are interrupted after the
configured timeout (default 120s).`
}

func (b *BashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute. Use && between commands that must all succeed.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "A short note describing what the command does and why. Useful for the log.",
			},
		},
		"required": []string{"command"},
	}
}

func (b *BashTool) Run(ctx *Context) (string, error) {
	command := StringArg(ctx.Args, "command", "")
	if command == "" {
		return "", fmt.Errorf("bash: empty command")
	}
	// Interactive commands cannot be driven non-interactively (Claude Code
	// refuses these too).
	if err := sandbox.CheckInteractive(command); err != nil {
		return "", err
	}
	// Sandbox command policy (strict mode blocks workspace escapes + network).
	if ctx.Sandbox != nil {
		if err := ctx.Sandbox.CommandPolicy(command); err != nil {
			return "", err
		}
	}
	cmdline := command
	if ctx.Sandbox != nil {
		// Resource limits (ulimit prefix) and, on macOS with strict mode, a
		// sandbox-exec profile wrap the command.
		if p := ctx.Sandbox.Prefix(); p != "" {
			cmdline = p + " " + cmdline
		}
		if wrapped := ctx.Sandbox.WrapCommand(cmdline); wrapped != "" {
			cmdline = wrapped
		}
	}
	res, err := RunShell(ctx, cmdline)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(res.Output)
	if !strings.HasSuffix(res.Output, "\n") && res.Output != "" {
		sb.WriteString("\n")
	}
	if res.Timeout {
		fmt.Fprintf(&sb, "\nExit code: %d (timed out)\n", res.ExitCode)
	} else {
		fmt.Fprintf(&sb, "\nExit code: %d\n", res.ExitCode)
	}
	return sb.String(), nil
}
