package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"ccdp/internal/atomicfile"
	"ccdp/internal/execution"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
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
	if !readOnly {
		if a.inPlanMode() {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "mutating external commands are unavailable in plan mode")
		}
	}
	if err := a.precheckCommand("Bash", map[string]any{"command": command}, permissions.KindShell); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	return a.scheduleCommandOperation(cmd, ext.Program, func(ctx context.Context) (string, error) {
		var authorizationErr error
		if readOnly && argv[0] == "git" {
			authorizationErr = a.authorizeGitQuery(ctx, cmd.ID, argv)
		} else {
			authorizationErr = a.authorizeCommand(ctx, cmd.ID, "Bash", map[string]any{"command": command}, permissions.KindShell)
		}
		if authorizationErr != nil {
			return "", authorizationErr
		}
		a.mu.Lock()
		sb, dir := a.services.sandbox, a.services.cfg.Workspace
		a.mu.Unlock()
		env := execution.SanitizedEnvironment(os.Environ())
		request := execution.Request{Context: ctx, Dir: dir, Sandbox: sb, Env: env, OutputLimit: commandOutputLimit}
		var res execution.Result
		var err error
		if readOnly && argv[0] == "git" {
			res, err = execution.RunReadOnlyGit(ctx, argv, request)
		} else {
			res, err = execution.RunArgv(ctx, argv, request)
		}
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
			receipt, err := a.admitInputFromWorker(protocol.Command{ID: cmd.ID, SessionID: cmd.SessionID,
				Type: protocol.CommandSubmitInput, Input: &protocol.SubmitInput{ID: protocol.InputID(cmd.ID), Text: text, Strategy: protocol.InputSteer}})
			if err != nil {
				return "", err
			}
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
		if err := a.precheckCommand("Bash", args, permissions.KindShell); err != nil {
			return "", err
		}
		if err := a.authorizeCommand(ctx, cmd.ID, "Bash", args, permissions.KindShell); err != nil {
			return "", err
		}
		a.mu.Lock()
		sb, dir := a.services.sandbox, a.services.cfg.Workspace
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
	return a.scheduleCommandOperation(cmd, "checkpoint", func(ctx context.Context) (string, error) {
		switch cmd.Checkpoint.Action {
		case protocol.CheckpointList:
			points, e := a.recoveryPoints()
			if e != nil {
				return "", e
			}
			b, e := json.MarshalIndent(map[string]any{"recovery_points": points}, "", "  ")
			return string(b), e
		case protocol.CheckpointCreate:
			if a.inPlanMode() {
				return "", errors.New("checkpoint creation is unavailable in plan mode")
			}
			p, e := a.captureRecoveryPoint(ctx, string(cmd.ID), cmd.Checkpoint.Summary)
			if e != nil {
				return "", e
			}
			p.After = p.Before
			if _, e = a.commitAuxiliary(p.ID+"-sealed", p); e != nil {
				return "", e
			}
			return "checkpoint " + p.ID + " created; /rewind prepare " + p.ID + " --mode both previews recovery", nil
		case protocol.CheckpointRestore:
			id := cmd.Checkpoint.ID
			next := cmd
			next.Workflow = &protocol.WorkflowCommand{Kind: protocol.WorkflowRestore, Action: "prepare", ID: id, Mode: "code", Scope: "snapshot"}
			return a.runRestore(ctx, next)
		default:
			return "", errors.New("unknown checkpoint action")
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
		path = "ccdp-export-" + a.conversation.sessionID + ".md"
	}
	resolved, err := a.commandPathLocked(path, true)
	a.mu.Unlock()
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	args := map[string]any{"file_path": resolved}
	if err := a.precheckCommand("Write", args, permissions.KindWrite); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	return a.scheduleCommandOperation(cmd, "export", func(ctx context.Context) (string, error) {
		if err := a.authorizeCommand(ctx, cmd.ID, "Write", args, permissions.KindWrite); err != nil {
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
	path, err := a.commandPathLocked(filepath.Join(a.services.cfg.Workspace, workspace.InstructionFile), true)
	a.mu.Unlock()
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	args := map[string]any{"file_path": path}
	if err := a.precheckCommand("Write", args, permissions.KindWrite); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	return a.scheduleCommandOperation(cmd, "init", func(ctx context.Context) (string, error) {
		if err := a.authorizeCommand(ctx, cmd.ID, "Write", args, permissions.KindWrite); err != nil {
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
		if cmd.Workflow.Action != "" {
			return a.scheduleOperation(cmd, "review", func(ctx context.Context) (string, error) { return a.readReviews(cmd.Workflow) }, operationConcurrent)
		}
		return a.scheduleOperation(cmd, "review", func(ctx context.Context) (string, error) { return a.launchReview(cmd) }, operationConcurrent)
	case protocol.WorkflowRestore:
		if a.inPlanMode() && cmd.Workflow.Action != "list" && cmd.Workflow.Action != "prepare" {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "restore is unavailable in plan mode")
		}
		return a.scheduleCommandOperation(cmd, "restore", func(ctx context.Context) (string, error) { return a.runRestore(ctx, cmd) })
	case protocol.WorkflowMerge:
		return a.scheduleCommandOperation(cmd, "merge child", func(ctx context.Context) (string, error) { return a.runMergeChild(ctx, cmd) })
	case protocol.WorkflowIndex:
		return a.scheduleCommandOperation(cmd, "index", func(ctx context.Context) (string, error) {
			a.mu.Lock()
			root := a.services.cfg.Workspace
			policy := a.services.sandbox.Snapshot()
			a.mu.Unlock()
			if cmd.Workflow.Action == "refresh" {
				workspace.Invalidate(root)
			}
			paths, err := workspace.Inventory(ctx, root, policy)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%d files in the complete accessible inventory. Use Glob/LS for paths and Rg for text. CodeNavigate supports outline, definition and references when a trusted language server is configured.", len(paths)), nil
		})
	case protocol.WorkflowCommitPushPR:
		if a.inPlanMode() {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "commit/push/PR is unavailable in plan mode")
		}
		return a.scheduleCommandOperation(cmd, "delivery", func(ctx context.Context) (string, error) {
			if cmd.Workflow.Action == "apply" || cmd.Workflow.Action == "resume" {
				return a.applyDelivery(ctx, cmd)
			}
			return a.prepareDelivery(ctx, cmd)
		})
	default:
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, fmt.Sprintf("unknown workflow %q", cmd.Workflow.Kind))
	}
}

func boundedCommandOutput(value string) string {
	return truncateResult(value, commandOutputLimit)
}

func (a *Agent) precheckCommand(toolName string, args map[string]any, kind permissions.ToolKind) error {
	a.mu.Lock()
	perms := a.services.perms
	a.mu.Unlock()
	if perms == nil {
		return errors.New("permission manager is unavailable")
	}
	if denied, reason := perms.HardDeny(toolName, args); denied {
		return errors.New(reason)
	}
	decision, reason := perms.Check(toolName, args, kind)
	if decision == permissions.DecisionDeny {
		return errors.New(reason)
	}
	return nil
}

func (a *Agent) authorizeCommand(ctx context.Context, id protocol.CommandID, toolName string, args map[string]any, kind permissions.ToolKind) error {
	a.mu.Lock()
	perms := a.services.perms
	a.mu.Unlock()
	if perms == nil {
		return errors.New("permission manager is unavailable")
	}
	if denied, reason := perms.HardDeny(toolName, args); denied {
		return errors.New(reason)
	}
	decision, reason := perms.Check(toolName, args, kind)
	return a.authorizeCommandDecision(ctx, id, toolName, args, decision, reason)
}

func (a *Agent) authorizeGitQuery(ctx context.Context, id protocol.CommandID, argv []string) error {
	a.mu.Lock()
	perms := a.services.perms
	a.mu.Unlock()
	decision, reason := perms.CheckReadOnlyGit(argv)
	return a.authorizeCommandDecision(ctx, id, "Bash", map[string]any{"command": formatExternalCommand(argv)}, decision, reason)
}

func (a *Agent) authorizeCommandDecision(ctx context.Context, id protocol.CommandID, toolName string, args map[string]any, decision permissions.Decision, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if a.services.sandbox == nil {
		return "", errors.New("sandbox policy is unavailable")
	}
	if write {
		resolved, err = a.services.sandbox.ResolveWrite(path)
	} else {
		resolved, err = a.services.sandbox.ResolveRead(path)
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
	protected := []string{a.services.cfg.SessionDir, filepath.Join(a.services.cfg.Workspace, ".ccdp")}
	if source := a.services.cfg.SourcePath(); source != "" {
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
	return permissions.ReadOnlyGitArguments(argv)
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
