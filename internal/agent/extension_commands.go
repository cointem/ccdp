package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ccdp/internal/atomicfile"
	"ccdp/internal/execution"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/workspace"
)

const (
	// commandFileLimit bounds the /apply source file. Unified patches stay on
	// this file path and are not converted into SubmitInput.Text; textual plans
	// are additionally subject to protocol.MaxSubmitInputTextBytes when they
	// enter the normal user-turn path.
	commandFileLimit   = 16 << 20
	commandOutputLimit = 512 << 10
)

// applyExternalCommand is the runtime endpoint for /git and the deliberately
// narrow GitHub command adapter. It accepts only argv values from protocol;
// there is no shell-string escape hatch at this boundary.
func (a *Agent) applyExternalCommand(cmd protocol.Command) protocol.Receipt {
	ext := cmd.External
	if ext == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "external command payload is required")
	}
	argv := append([]string{ext.Program}, ext.Args...)
	command := formatExternalCommand(argv)
	readOnly := externalCommandReadOnly(argv)
	actualArgv := append([]string(nil), argv...)
	if readOnly && argv[0] == "git" {
		hardened, err := execution.ReadOnlyGitArgv(argv)
		if err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
		}
		actualArgv = hardened
	}
	if !readOnly {
		if a.inPlanMode() {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "mutating external commands are unavailable in plan mode")
		}
	}
	if err := a.precheckCommand("Bash", map[string]any{"command": command}); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	return a.scheduleCommandOperation(cmd, ext.Program, func(ctx context.Context) (string, error) {
		if err := a.authorizeCommand(ctx, cmd.ID, "Bash", map[string]any{"command": command}); err != nil {
			return "", err
		}
		a.mu.Lock()
		sb, dir := a.sandbox, a.cfg.Workspace
		a.mu.Unlock()
		env := execution.SanitizedEnvironment(os.Environ())
		if readOnly && actualArgv[0] == "git" {
			env = execution.ReadOnlyGitEnvironment(env)
		}
		res, err := execution.RunArgv(ctx, actualArgv, execution.Request{Context: ctx, Dir: dir,
			Sandbox: sb, Env: env, OutputLimit: commandOutputLimit})
		if err != nil {
			return strings.TrimSpace(res.Output), err
		}
		if res.TimedOut {
			return strings.TrimSpace(res.Output), errors.New("external command timed out")
		}
		if res.ExitCode != 0 {
			output := strings.TrimSpace(res.Output)
			return output, externalCommandFailure(ext.Program, res.ExitCode, output)
		}
		return strings.TrimSpace(res.Output), nil
	})
}

// externalCommandFailure explains a non-zero exit with the command's own first
// line of output. Without it a failure surfaces as an opaque "exited with
// status N": for example `git diff` run outside a repository exits 129 and only
// writes "Not a git repository" to the captured output, which the status code
// alone never conveys.
func externalCommandFailure(program string, exitCode int, output string) error {
	if strings.Contains(output, "Not a git repository") {
		return fmt.Errorf("%s failed: workspace is not a git repository", program)
	}
	if reason := firstOutputLine(output); reason != "" {
		return fmt.Errorf("%s exited with status %d: %s", program, exitCode, reason)
	}
	return fmt.Errorf("%s exited with status %d", program, exitCode)
}

// firstOutputLine returns the first non-empty line of output, trimmed to a
// length that stays readable in a single status row.
func firstOutputLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateRunes(line, 200)
		}
	}
	return ""
}

func truncateRunes(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "…"
}

// applyApplyCommand reads a bounded plan/patch file in the runtime so the TUI
// never performs a second execution path. A textual plan becomes an ordinary
// user input; a unified patch is applied by the same gated execution runner as
// every other git mutation.
func (a *Agent) applyApplyCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.Apply == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "apply payload is required")
	}
	a.mu.Lock()
	path, err := a.commandPathLocked(cmd.Apply.Path, false)
	a.mu.Unlock()
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	if err := validateCommandFile(path, commandFileLimit); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, fmt.Sprintf("read apply file: %v", err))
	}
	return a.scheduleCommandOperation(cmd, "apply", func(ctx context.Context) (string, error) {
		content, err := readBoundedCommandFile(ctx, path, commandFileLimit)
		if err != nil {
			return "", fmt.Errorf("read apply file: %w", err)
		}
		if !looksLikeUnifiedPatch(content) {
			text := "Apply the following plan in the current workspace, following normal approval and sandbox policy.\n\n" + content
			receipt := a.applySubmitInput(protocol.Command{ID: cmd.ID, SessionID: cmd.SessionID,
				Type: protocol.CommandSubmitInput, Input: &protocol.SubmitInput{ID: protocol.InputID(cmd.ID), Text: text, Strategy: protocol.InputSteer}})
			if receipt.Rejected() {
				if receipt.Error != nil {
					return "", receipt.Error
				}
				return "", errors.New("apply plan was rejected")
			}
			return "plan submitted for normal approval", nil
		}
		if a.inPlanMode() {
			return "", errors.New("patch application is unavailable in plan mode")
		}
		args := map[string]any{"command": "git apply --whitespace=nowarn -"}
		if err := a.precheckCommand("Bash", args); err != nil {
			return "", err
		}
		if err := a.authorizeCommand(ctx, cmd.ID, "Bash", args); err != nil {
			return "", err
		}
		a.mu.Lock()
		sb, dir := a.sandbox, a.cfg.Workspace
		a.mu.Unlock()
		res, err := execution.RunArgv(ctx, []string{"git", "apply", "--whitespace=nowarn", "-"}, execution.Request{
			Context: ctx, Dir: dir, Sandbox: sb, Input: strings.NewReader(content),
			Env: execution.SanitizedEnvironment(os.Environ()), OutputLimit: commandOutputLimit})
		if err != nil {
			return strings.TrimSpace(res.Output), err
		}
		if res.TimedOut {
			return strings.TrimSpace(res.Output), errors.New("git apply timed out")
		}
		if res.ExitCode != 0 {
			return strings.TrimSpace(res.Output), fmt.Errorf("git apply exited with status %d", res.ExitCode)
		}
		return strings.TrimSpace(res.Output), nil
	})
}

func (a *Agent) applyCheckpointCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.Checkpoint == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "checkpoint payload is required")
	}
	action := cmd.Checkpoint.Action
	if action == protocol.CheckpointList {
		return a.scheduleCommandOperation(cmd, "checkpoint", func(context.Context) (string, error) {
			records := a.CheckpointList()
			if len(records) == 0 {
				return "no checkpoints", nil
			}
			var b strings.Builder
			for _, record := range records {
				fmt.Fprintf(&b, "%s  %s  %s\n", record.ID, record.CreatedAt.Format(time.RFC3339), record.Summary)
			}
			return strings.TrimSpace(b.String()), nil
		})
	}
	if a.inPlanMode() {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "checkpoint mutation is unavailable in plan mode")
	}
	command := "git stash create -u ccdp checkpoint"
	if action == protocol.CheckpointRestore {
		command = "git checkout <checkpoint> -- . && git reset -q <checkpoint> --"
	}
	if err := a.precheckCommand("Bash", map[string]any{"command": command}); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	return a.scheduleCommandOperation(cmd, "checkpoint", func(ctx context.Context) (string, error) {
		if err := a.authorizeCommand(ctx, cmd.ID, "Bash", map[string]any{"command": command}); err != nil {
			return "", err
		}
		a.mu.Lock()
		dir := a.cfg.Workspace
		a.mu.Unlock()
		switch action {
		case protocol.CheckpointCreate:
			id, err := a.checkpoints.CreateContext(ctx, dir, cmd.Checkpoint.Summary)
			if err != nil {
				return "", err
			}
			if id == "" {
				return "no changes to checkpoint", nil
			}
			return "checkpoint " + id + " created", nil
		case protocol.CheckpointRestore:
			if err := a.checkpoints.RestoreContext(ctx, dir, cmd.Checkpoint.ID); err != nil {
				return "", err
			}
			workspace.Invalidate(dir)
			return "restored checkpoint " + cmd.Checkpoint.ID, nil
		default:
			return "", fmt.Errorf("unknown checkpoint action %q", action)
		}
	})
}

func (a *Agent) applyExportCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.Export == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "export payload is required")
	}
	if a.inPlanMode() {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "export is unavailable in plan mode")
	}
	a.mu.Lock()
	path := cmd.Export.Path
	if path == "" {
		path = "ccdp-export-" + a.sessionID + ".md"
	}
	resolved, err := a.commandPathLocked(path, true)
	a.mu.Unlock()
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	args := map[string]any{"file_path": resolved}
	if err := a.precheckCommand("Write", args); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	return a.scheduleCommandOperation(cmd, "export", func(ctx context.Context) (string, error) {
		if err := a.authorizeCommand(ctx, cmd.ID, "Write", args); err != nil {
			return "", err
		}
		data := []byte(a.ExportMarkdown())
		if err := atomicfile.WriteFile(resolved, data, 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("exported %d bytes to %s", len(data), resolved), nil
	})
}

func (a *Agent) applyInitCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.Init == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "init payload is required")
	}
	if a.inPlanMode() {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "init is unavailable in plan mode")
	}
	a.mu.Lock()
	path, err := a.commandPathLocked(filepath.Join(a.cfg.Workspace, workspace.InstructionFile), true)
	a.mu.Unlock()
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	args := map[string]any{"file_path": path}
	if err := a.precheckCommand("Write", args); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	return a.scheduleCommandOperation(cmd, "init", func(ctx context.Context) (string, error) {
		if err := a.authorizeCommand(ctx, cmd.ID, "Write", args); err != nil {
			return "", err
		}
		if _, err := os.Lstat(path); err == nil {
			return "", fmt.Errorf("%s already exists", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		ok := false
		defer func() {
			_ = f.Close()
			if !ok {
				_ = os.Remove(path)
			}
		}()
		if _, err := io.WriteString(f, workspace.InstructionTemplate); err != nil {
			return "", err
		}
		if err := f.Sync(); err != nil {
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		ok = true
		return "wrote " + path + " (edit it with project guidance)", nil
	})
}

// applyWorkflowCommand is the runtime-owned entry point for workspace/GitHub
// workflows.  The UI submits one semantic command; the sole runtime worker
// owns the ordered argv list, approval boundary, cancellation context and
// durable completion.  No workflow step is launched by a TUI command chain.
func (a *Agent) applyWorkflowCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.Workflow == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "workflow payload is required")
	}
	switch cmd.Workflow.Kind {
	case protocol.WorkflowReview:
		return a.scheduleCommandOperation(cmd, "review", func(ctx context.Context) (string, error) {
			diff, err := a.runWorkflowStep(ctx, cmd.ID, "git-diff", []string{"git", "diff"}, false)
			if err != nil {
				return diff, err
			}
			records := a.CheckpointList()
			var report strings.Builder
			report.WriteString(diff)
			if len(records) > 0 {
				report.WriteString("\n\ncheckpoints:\n")
				for _, record := range records {
					fmt.Fprintf(&report, "%s  %s  %s\n", record.ID, record.CreatedAt.Format(time.RFC3339), record.Summary)
				}
			}
			return strings.TrimSpace(report.String()), nil
		})
	case protocol.WorkflowCommitPushPR:
		if a.inPlanMode() {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "commit/push/PR is unavailable in plan mode")
		}
		message := strings.TrimSpace(cmd.Workflow.Message)
		steps := []struct {
			label   string
			argv    []string
			network bool
		}{
			{label: "git-add", argv: []string{"git", "add", "-A"}},
			{label: "git-commit", argv: []string{"git", "commit", "-m", message}},
			{label: "git-push", argv: []string{"git", "push", "-u", "origin", "HEAD"}, network: true},
			{label: "gh-pr-create", argv: []string{"gh", "pr", "create", "--fill"}, network: true},
		}
		return a.scheduleCommandOperation(cmd, "commit/push/PR", func(ctx context.Context) (string, error) {
			var report strings.Builder
			for i, step := range steps {
				id := protocol.CommandID(fmt.Sprintf("%s-step-%d", cmd.ID, i+1))
				out, err := a.runWorkflowStep(ctx, id, step.label, step.argv, step.network)
				if out != "" {
					if report.Len() > 0 {
						report.WriteString("\n")
					}
					fmt.Fprintf(&report, "%s: %s", step.label, out)
				}
				if err != nil {
					return strings.TrimSpace(report.String()), err
				}
			}
			return strings.TrimSpace(report.String()), nil
		})
	default:
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, fmt.Sprintf("unknown workflow %q", cmd.Workflow.Kind))
	}
}

// runWorkflowStep performs one fixed, argv-only step after the normal hard
// deny/approval gate.  It intentionally rechecks policy for every step so a
// deny/reload cannot be bypassed by an earlier approval in the same workflow.
func (a *Agent) runWorkflowStep(ctx context.Context, id protocol.CommandID, label string, argv []string, network bool) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("%s: empty command", label)
	}
	if argv[0] != "git" && argv[0] != "gh" {
		return "", fmt.Errorf("%s: command is not allowed", label)
	}
	command := formatExternalCommand(argv)
	if err := a.precheckCommand("Bash", map[string]any{"command": command}); err != nil {
		return "", err
	}
	if err := a.authorizeCommand(ctx, id, "Bash", map[string]any{"command": command}); err != nil {
		return "", err
	}
	a.mu.Lock()
	workspace := a.cfg.Workspace
	sb := a.sandbox
	a.mu.Unlock()
	if network && (sb == nil || !sb.NetworkAllowed()) {
		return "", fmt.Errorf("%s: network command denied: no network capability is authorized", label)
	}
	actualArgv := argv
	readOnly := externalCommandReadOnly(argv)
	if readOnly && argv[0] == "git" {
		hardened, err := execution.ReadOnlyGitArgv(argv)
		if err != nil {
			return "", err
		}
		actualArgv = hardened
	}
	env := execution.SanitizedEnvironmentFor(execution.EnvironmentGit, os.Environ())
	if readOnly && actualArgv[0] == "git" {
		env = execution.ReadOnlyGitEnvironment(env)
	}
	res, err := execution.RunArgv(ctx, actualArgv, execution.Request{Context: ctx, Dir: workspace,
		Sandbox: sb, Env: env, OutputLimit: commandOutputLimit})
	out := strings.TrimSpace(res.Output)
	if err != nil {
		return out, fmt.Errorf("%s: %w", label, err)
	}
	if res.TimedOut {
		return out, fmt.Errorf("%s: command timed out", label)
	}
	if res.ExitCode != 0 {
		return out, fmt.Errorf("%s: exited with status %d", label, res.ExitCode)
	}
	return out, nil
}

type commandOperation func(context.Context) (string, error)

func (a *Agent) scheduleCommandOperation(cmd protocol.Command, name string, operation commandOperation) protocol.Receipt {
	return a.scheduleCommandOperationWithBusy(cmd, name, operation, true)
}

// scheduleCommandOperationWithBusy runs a typed operation with one durable
// completion/receipt boundary. Read-only queries and an explicit Save may run
// alongside a model turn; mutating operations reserve the session busy state.
func (a *Agent) scheduleCommandOperationWithBusy(cmd protocol.Command, name string, operation commandOperation, requireIdle bool) protocol.Receipt {
	a.mu.Lock()
	revoking := a.capabilityRevoking
	if (requireIdle && (a.busy || a.settling)) || a.closing || a.closed ||
		revoking && name != "query:doctor" && name != "query:status" {
		a.mu.Unlock()
		if revoking {
			return a.rejectedReceipt(cmd, protocol.ErrorBusy, "sandbox capability revocation is in progress")
		}
		if requireIdle {
			return a.rejectedReceipt(cmd, protocol.ErrorBusy, "command requires an idle session")
		}
		return a.rejectedReceipt(cmd, protocol.ErrorClosed, "session is closing")
	}
	ctx, cancel := context.WithCancel(a.rootCtx)
	if requireIdle {
		a.busy = true
		a.phase = protocol.PhaseExecutingTools
		a.turnCancel = cancel
		a.turnCtx = ctx
	}
	a.operationWG.Add(1)
	trackForSandboxQuiescence := !sandboxQuiescenceOwner(name) && name != "query:doctor" && name != "query:status"
	if trackForSandboxQuiescence {
		a.sandboxOperationWG.Add(1)
	}
	a.mu.Unlock()
	if err := a.persistCommandScheduled(cmd, name, a.revision().LogSeq); err != nil {
		if requireIdle {
			a.mu.Lock()
			if a.turnCtx == ctx {
				a.turnCancel = nil
				a.turnCtx = nil
				a.busy = false
				a.phase = protocol.PhaseIdle
			}
			a.mu.Unlock()
		}
		cancel()
		a.operationWG.Done()
		if trackForSandboxQuiescence {
			a.sandboxOperationWG.Done()
		}
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	if requireIdle {
		a.publishState()
	}
	receipt := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
	go a.completeScheduledOperation(ctx, cancel, cmd, name, operation, requireIdle, trackForSandboxQuiescence)
	return receipt
}

// completeScheduledOperation runs a long-lived command operation, releases its
// busy reservation, and persists the terminal CommandCompleted admission ahead
// of the final receipt so a restart cannot observe a success without it.
func (a *Agent) completeScheduledOperation(ctx context.Context, cancel context.CancelFunc, cmd protocol.Command, name string, operation func(context.Context) (string, error), requireIdle, trackForSandboxQuiescence bool) {
	defer a.operationWG.Done()
	if trackForSandboxQuiescence {
		defer a.sandboxOperationWG.Done()
	}
	output, err := operation(ctx)
	status := "success"
	if err != nil {
		status = "error"
	}
	boundedOutput := boundedCommandOutput(output)
	if requireIdle {
		a.mu.Lock()
		// context.CancelFunc is deliberately not comparable. The context returned
		// by WithCancel is the operation's identity, so clear the cancellation
		// handles only if this worker still owns that context.
		if a.turnCtx == ctx {
			a.turnCancel = nil
			a.turnCtx = nil
		}
		if !a.closing && !a.closed {
			a.busy = false
			a.phase = protocol.PhaseIdle
		}
		a.mu.Unlock()
	}
	cancel()
	// receipt persists CommandCompleted before publishing the final receipt.
	// Keep that durable boundary ahead of all transient operation output so a
	// restart cannot observe a successful report without its completion fact.
	completion := &session.CommandCompleted{CommandID: string(cmd.ID), Outcome: status}
	if err != nil {
		completion.Code = string(protocol.ErrorInternal)
		completion.Report = err.Error()
	}
	finalReceipt := a.receiptWithFactAndOutput(cmd, func() protocol.ReceiptStatus {
		if err != nil {
			return protocol.ReceiptRejected
		}
		return protocol.ReceiptApplied
	}(), protocol.OperationID(cmd.ID), func() *protocol.CommandError {
		if err == nil {
			return nil
		}
		return &protocol.CommandError{Code: protocol.ErrorInternal, Message: err.Error()}
	}(), completion, boundedOutput)
	if finalReceipt.Rejected() {
		reason := "command completion was not persisted"
		if finalReceipt.Error != nil {
			reason = finalReceipt.Error.Error()
		}
		a.emit(Event{Type: EventError, Text: name + ": " + reason})
	} else {
		a.emit(Event{Type: EventToolResult, Tool: &ToolEvent{ID: string(cmd.ID), Name: name, Status: status, Output: boundedOutput}})
		if err != nil {
			a.emit(Event{Type: EventError, Text: name + ": " + err.Error()})
		}
	}
	a.publishState()
}

func boundedCommandOutput(value string) string {
	return truncateResult(value, commandOutputLimit)
}

func (a *Agent) precheckCommand(toolName string, args map[string]any) error {
	a.mu.Lock()
	perms := a.perms
	a.mu.Unlock()
	if perms == nil {
		return errors.New("permission manager is unavailable")
	}
	if denied, reason := perms.HardDeny(toolName, args); denied {
		return errors.New(reason)
	}
	decision, reason := perms.Check(toolName, args)
	if decision == permissions.DecisionDeny {
		return errors.New(reason)
	}
	return nil
}

func (a *Agent) authorizeCommand(ctx context.Context, id protocol.CommandID, toolName string, args map[string]any) error {
	a.mu.Lock()
	perms := a.perms
	a.mu.Unlock()
	if perms == nil {
		return errors.New("permission manager is unavailable")
	}
	if denied, reason := perms.HardDeny(toolName, args); denied {
		return errors.New(reason)
	}
	decision, reason := perms.Check(toolName, args)
	if decision == permissions.DecisionDeny {
		return errors.New(reason)
	}
	if decision != permissions.DecisionAsk {
		return nil
	}
	call := messages.ToolCall{ID: string(id), Name: toolName, Arguments: args}
	approved, _ := a.requestApproval(call, reason)
	if !approved {
		return errors.New("command was not approved")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (a *Agent) commandPathLocked(path string, write bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	var resolved string
	var err error
	if a.sandbox == nil {
		return "", errors.New("sandbox policy is unavailable")
	}
	if write {
		resolved, err = a.sandbox.ResolveWrite(path)
	} else {
		resolved, err = a.sandbox.ResolveRead(path)
	}
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
	}
	resolved = filepath.Clean(resolved)
	protected := []string{a.cfg.SessionDir, filepath.Join(a.cfg.Workspace, ".ccdp")}
	if source := a.cfg.SourcePath(); source != "" {
		protected = append(protected, source)
	}
	for _, root := range protected {
		if commandPathWithin(root, resolved) {
			return "", fmt.Errorf("path %s is a protected runtime location", path)
		}
	}
	return resolved, nil
}

func commandPathWithin(root, path string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}
	if info, statErr := os.Stat(root); statErr == nil && !info.IsDir() {
		return filepath.Clean(root) == filepath.Clean(path)
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validateCommandFile(path string, limit int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("file is not a regular file")
	}
	if info.Size() > limit {
		return fmt.Errorf("file exceeds %d bytes", limit)
	}
	return nil
}

func readBoundedCommandFile(ctx context.Context, path string, limit int64) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	// O_NONBLOCK prevents a path swapped to a FIFO after the admission stat
	// from wedging the command worker. Fstat closes the race before reading.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr != nil {
		return "", statErr
	} else if !info.Mode().IsRegular() {
		return "", fmt.Errorf("file is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("file exceeds %d bytes", limit)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	return string(data), nil
}

func formatExternalCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = shellQuoteCommand(arg)
	}
	return strings.Join(parts, " ")
}

func shellQuoteCommand(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func externalCommandReadOnly(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	if argv[0] == "gh" {
		if len(argv) < 3 {
			return len(argv) == 2 && argv[1] == "--version"
		}
		if !((argv[1] == "pr" && (argv[2] == "view" || argv[2] == "list")) ||
			(argv[1] == "issue" && (argv[2] == "view" || argv[2] == "list")) ||
			(argv[1] == "repo" && argv[2] == "view")) {
			return false
		}
		return !hasExternalWriteFlag(argv[3:])
	}
	switch argv[1] {
	case "status":
		return gitStatusReadOnlyArgs(argv[2:])
	case "diff":
		// --output/-o writes a file even though the git subcommand is usually
		// read-only. External commands are argv-only, so reject both forms.
		return gitReadOnlyArgs(argv[2:], true)
	case "log", "show", "rev-parse", "ls-files", "describe":
		return gitReadOnlyArgs(argv[2:], false)
	case "branch":
		return gitBranchReadOnlyArgs(argv[2:])
	case "remote":
		// `--` must precede the remote name when present. Do not accept
		// arbitrary remote subcommands or a write-capable remote operation.
		return len(argv) == 4 && argv[2] == "get-url" && argv[3] == "origin" ||
			len(argv) == 5 && argv[2] == "get-url" && argv[3] == "--" && argv[4] == "origin"
	default:
		return false
	}
}

func hasExternalWriteFlag(args []string) bool {
	for _, arg := range args {
		switch {
		case arg == "-o", arg == "--output", strings.HasPrefix(arg, "--output="):
			return true
		case arg == "--web", arg == "--edit", arg == "--delete-branch", arg == "--merge", arg == "--close", arg == "--reopen", arg == "--approve", arg == "--comment", arg == "--add-label", arg == "--remove-label":
			return true
		}
	}
	return false
}

func gitStatusReadOnlyArgs(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			// Pathspecs after -- are read-only.
			return true
		}
		if strings.HasPrefix(arg, "-") {
			switch arg {
			case "--short", "-s", "--porcelain", "--porcelain=v1", "--porcelain=v2", "--branch", "--show-stash", "--ahead-behind", "--no-renames", "--renames", "--ignored", "--untracked-files", "-u", "--untracked-files=all", "--untracked-files=normal", "--untracked-files=no":
				continue
			default:
				return false
			}
		}
	}
	return true
}

func gitBranchReadOnlyArgs(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if strings.HasPrefix(arg, "-") {
			switch arg {
			case "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "--list", "-l", "--show-current", "--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--format", "--column", "--sort", "--color", "--no-color":
				continue
			default:
				return false
			}
		}
		// A bare branch name creates a branch; no positional arguments are
		// accepted by the read-only adapter.
		return false
	}
	return true
}

func gitReadOnlyArgs(args []string, diff bool) bool {
	if hasExternalWriteFlag(args) {
		return false
	}
	for _, arg := range args {
		if arg == "--no-index" || arg == "--ext-diff" || arg == "--textconv" {
			return false
		}
		if diff && (arg == "--output" || strings.HasPrefix(arg, "--output=")) {
			return false
		}
	}
	return true
}

func looksLikeUnifiedPatch(content string) bool {
	lines := strings.Split(content, "\n")
	if len(lines) > 8 {
		lines = lines[:8]
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			return true
		}
	}
	return false
}
