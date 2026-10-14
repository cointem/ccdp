package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

// runTool executes an approved tool call, publishing the tool pipeline domain
// events and running PostToolUse + Go post hooks.
func (a *Agent) runTool(tc messages.ToolCall, tool tools.Tool) (string, bool) {
	return a.runToolWithJournal(tc, tool, toolJournalContext{}, nil)
}

func (a *Agent) runToolWithJournal(tc messages.ToolCall, tool tools.Tool, journal toolJournalContext, execution *toolJournalExecution) (string, bool) {
	return a.runToolWithJournalPolicy(tc, tool, journal, execution, nil)
}

func (a *Agent) runToolWithJournalPolicy(tc messages.ToolCall, tool tools.Tool, journal toolJournalContext, execution *toolJournalExecution, invocationPolicy *sandbox.Sandbox) (string, bool) {
	a.mu.Lock()
	cfg := cloneConfig(a.services.cfg)
	turnCtx := a.turn.turnCtx
	sb := invocationPolicy
	if sb == nil {
		sb = a.services.sandbox
	}
	if sb != nil {
		sb = sb.Snapshot()
	}
	resources := a.services.resources
	skills := a.services.skills
	a.mu.Unlock()
	if turnCtx == nil {
		turnCtx = a.lifecycle.rootCtx
	}
	if err := a.toolExecutionError(); err != nil {
		return err.Error(), true
	}
	// Guardian review (Codex's guardian): high-risk calls are reviewed by a
	// read-only sub-agent before executing. Enabled via enable_guardian.
	if cfg.GuardianEnabled() && toolKind(tool).NeedsReview() {
		if err := a.guardianCheck(tc); err != nil {
			a.emit(toolEvent(tc, "denied", err.Error()))
			return err.Error(), true
		}
	}
	if err := a.toolExecutionError(); err != nil {
		return err.Error(), true
	}
	if err := a.admitToolStart(tc, tool, execution); err != nil {
		return err.Error(), true
	}
	finishToolOutput := func(raw string, failed bool) (string, bool) {
		if execution != nil {
			if err := execution.finish(a, tc, raw, failed); err != nil {
				return "session persistence failed: " + err.Error(), true
			}
			preview := a.toolOutputPreview(raw, cfg.MaxResultSizeChars)
			if preview != raw && json.Valid([]byte(raw)) {
				failed = true
			}
			if _, reader := tool.(*tools.ReadTool); reader && preview != raw && resources != nil {
				if path, e := sb.ResolveRead(tools.StringArg(tc.Arguments, "file_path", "")); e == nil {
					resources.Files.ForgetFile(path)
				}
			}
			return preview, failed
		}
		// Direct/legacy callers retain the historical output-file and
		// per-agent clipping behavior. Canonical turns use the typed raw blob
		// and apply aggregate clipping once, in call order, in dispatchTools.
		raw = a.maybePersistResult(tc, raw)
		return a.clipAggregate(raw), failed
	}
	a.services.evbus.Emit(events.TopicToolPreExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "running"})
	a.emit(Event{Type: EventToolStart, Tool: &ToolEvent{ID: tc.ID, Name: tc.Name, Args: tc.Arguments}})
	a.emit(toolEvent(tc, "running", ""))
	a.emitStatus("%s running…", tc.Name)
	start := time.Now()
	tctx := a.buildToolContext(tc, turnCtx, cfg, sb, resources, skills)
	if err := a.toolExecutionError(); err != nil {
		execution.setStatus("cancelled")
		return finishToolOutput(err.Error(), true)
	}
	out, err := tool.Run(tctx)
	if _, todo := tool.(*tools.TodoWriteTool); err == nil && todo {
		// TodoStore is a mutable session resource. The tool's own store write
		// completes first; only then publish the typed task replacement so a
		// replay can restore the list without consulting a sidecar file.
		if taskErr := a.persistTasksSnapshot(); taskErr != nil {
			err = taskErr
		}
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	status := "ok"
	if err != nil {
		status = err.Error()
	}
	a.logv("tool %s %s in %s", tc.Name, status, elapsed)

	// Invalidate the workspace model after anything that may have changed files.
	switch toolKind(tool) {
	case permissions.KindWrite, permissions.KindShell, permissions.KindProcess, permissions.KindUnknown:
		workspace.Invalidate(cfg.Workspace)
	}

	// AutoMem: remember files the turn modified so the memory log can say what
	// changed. Only Write/Edit carry a reliable file_path argument.
	if cfg.MemoryEnabled() {
		switch toolKind(tool) {
		case permissions.KindWrite:
			if p, _ := tc.Arguments["file_path"].(string); p != "" {
				a.recordTouched(p)
			}
		}
	}

	if err != nil {
		raw := fmt.Sprintf("%s failed after %s: %v", tc.Name, elapsed, err)
		if strings.TrimSpace(out) != "" {
			raw += "\n" + out
		}
		out, _ = finishToolOutput(raw, true)
		a.emit(toolEvent(tc, "error", out))
		a.services.evbus.Emit(events.TopicToolPostExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "error", Error: err.Error()})
		// PostToolUseFailure hooks observe the failure (non-blocking).
		a.runHookWithJournal(turnCtx, hooks.EventPostToolUseFailure, func(hctx context.Context) hooks.Output {
			return a.services.hooks.PostToolUseFailure(hctx, tc.Name, tc.Arguments, err.Error())
		})
		return out, true
	}

	// PostToolUse shell hooks can append context to the result.
	if ho := a.runHookWithJournal(turnCtx, hooks.EventPostToolUse, func(hctx context.Context) hooks.Output {
		return a.services.hooks.PostToolUse(hctx, tc.Name, tc.Arguments, out)
	}); ho.HookSpecificOutput != "" {
		out += "\n" + ho.HookSpecificOutput
	}
	// In-process Go post hooks run alongside.
	if extra := a.services.gohooks.RunPostTool(tc.Name, tc.Arguments, out); extra != "" {
		out += extra
	}
	out, _ = finishToolOutput(out, false)

	a.emit(toolEvent(tc, "success", out))
	a.services.evbus.Emit(events.TopicToolPostExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "success", Output: out})
	a.services.evbus.Emit(events.TopicToolResult, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "success", Output: out})
	return out, false
}

// buildToolContext assembles the *tools.Context handed to an approved tool,
// wiring Bash timeout, sub-agent/skills callbacks, session identity, and the
// streaming output sink. The registry/MCP leases are already frozen at the step
// boundary, so this context cannot observe a mid-stream plugin update.
func (a *Agent) buildToolContext(tc messages.ToolCall, turnCtx context.Context, cfg config.Config, sb *sandbox.Sandbox, resources *tools.Resources, skills *skills.Store) *tools.Context {
	sb = sb.Snapshot()
	var tctx *tools.Context
	if resources != nil {
		tctx = resources.Context(turnCtx, cfg.Workspace, sb)
	} else {
		// This branch is only for old hand-built Agent values in package tests.
		// New sessions always own a Resources container.
		tctx = &tools.Context{Context: turnCtx, WorkingDir: cfg.Workspace, SessionDir: cfg.SessionDir, Sandbox: sb}
	}
	if a.children.childState != nil && a.children.childState.purpose == childPurposeReview {
		tctx.ReadRoot = filepath.Dir(cfg.Workspace)
	}
	tctx.Args = make(map[string]any, len(tc.Arguments))
	for name, value := range tc.Arguments {
		if name == "requested_capabilities" {
			continue
		}
		tctx.Args[name] = value
	}
	tctx.HostNetworkAllowed = sb != nil && sb.NetworkAllowed()
	tctx.Timeout = cfg.BashTimeout()
	if cfg.MaxResultSizeChars > 0 {
		tctx.OutputLimit = cfg.MaxResultSizeChars
	}
	if a.children.childState == nil {
		tctx.Sessions = a.Sessions()
	}
	// Provider call IDs are response-local and may repeat on a later step.
	a.mu.Lock()
	turn, step := a.sequences.turnSeq, a.sequences.stepSeq
	goalID := a.conversation.stepGoalID
	a.mu.Unlock()
	tctx.Goals = goalToolControl{agent: a, goalID: goalID}
	tctx.AgentCommandID = protocol.CommandID(messages.StableID("agent-tool", struct {
		Session, Call string
		Turn, Step    uint64
	}{a.conversation.sessionID, tc.ID, turn, step}))
	tctx.SendAgentMessage = func(target, text string) error { return a.sendAgentMessage(turnCtx, tctx.AgentCommandID, target, text) }
	tctx.WaitAgentEvent = a.waitAgentEvent
	if a.services.supervisor != nil {
		tctx.ReadAgent = a.services.supervisor.readAgent
	}
	tctx.SpawnAgent = func(task tools.SpawnAgentRequest) (protocol.AgentReceipt, error) {
		return a.spawnAgent(task, tc.ID)
	}
	tctx.NavigateSemantic = func(op, path string, line, character int) (string, error) {
		return a.semanticNavigate(turnCtx, cfg, sb, op, path, line, character)
	}
	tctx.Skills = skills
	if a.persistenceHandle() != nil {
		tctx.BeforeWrite = a.beforeCodingWrite
	}
	if resources != nil {
		tctx.Owner = resources.Owner()
	}
	tctx.Notify = func(line string) {
		a.emit(Event{Type: EventToolStream, Tool: &ToolEvent{
			ID: tc.ID, Name: tc.Name, Status: "stream", Output: line,
		}})
	}
	return tctx
}
