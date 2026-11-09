package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"ccdp/internal/codemode"
	"ccdp/internal/config"
	"ccdp/internal/events"
	processExecution "ccdp/internal/execution"
	"ccdp/internal/fsops"
	"ccdp/internal/hooks"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
	"ccdp/internal/tooling"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

func (a *Agent) runToolWithJournalPolicy(tc messages.ToolCall, tool tools.Tool, journal toolJournalContext, execution *toolJournalExecution, invocationPolicy *sandbox.Sandbox) tooling.Result {
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
	if journal.config != nil {
		cfg = cloneConfig(journal.config)
	}
	if journal.context != nil {
		turnCtx = journal.context
	}
	if turnCtx == nil {
		turnCtx = a.lifecycle.rootCtx
	}
	if err := a.toolExecutionError(); err != nil {
		return tooling.Result{Output: err.Error(), IsError: true}
	}
	if err := a.admitToolStart(tc, tool, execution); err != nil {
		return tooling.Result{Output: err.Error(), IsError: true}
	}
	var codeResult *protocol.CodeResult
	var observeRead func(string)
	finishToolOutput := func(raw string, failed bool) tooling.Result {
		if codeResult != nil {
			encoded, err := json.Marshal(codeResult)
			if err != nil {
				return tooling.Result{Output: "encode code result: " + err.Error(), IsError: true}
			}
			raw = string(encoded)
		}
		preview := raw
		if !journal.program && codeResult == nil && execution.result.Process == nil {
			preview = a.toolOutputPreview(raw, cfg.MaxResultSizeBytes)
			execution.displayTruncated = preview != raw
		}
		execution.displayTruncated = execution.displayTruncated || execution.result.FullText != ""
		if err := execution.finish(a, tc, raw, failed); err != nil {
			return tooling.Result{Output: "session persistence failed: " + err.Error(), IsError: true}
		}
		if execution.displayTruncated {
			if execution.result.FullText != "" {
				// Preserve the tool's preview cap while using the shared output ID.
				raw += fmt.Sprintf("\n[incomplete; continue with ReadOutput output_id=%q offset=%d]", execution.outputID, len(raw))
			}
			preview = a.toolOutputPreview(raw, cfg.MaxResultSizeBytes, execution.outputID)
		}
		result := tooling.Result{ObserveOutput: observeRead, Process: execution.result.Process, CompletionIDs: execution.result.CompletionIDs, Output: preview, IsError: failed, Code: codeResult,
			Data: execution.result.Data, DataTruncated: execution.result.DataTruncated,
			DisplayTruncated: execution.displayTruncated, DataError: execution.result.DataError, OutputID: execution.outputID}
		if journal.program {
			if result.DataError != "" {
				// The original bytes remain in the journal, but invalid data is
				// not exposed as a usable JavaScript value.
				result.Data = nil
			}
			wire := codemode.Result{Failure: execution.failure, CallID: tc.ID, Status: execution.status, Text: raw,
				Data: result.Data, Truncated: result.DataTruncated, DataError: result.DataError, OutputID: result.OutputID}
			encoded, encodeErr := json.Marshal(wire)
			if encodeErr != nil || len(encoded) > codemode.MaxResult {
				result.Output = "Tool execution completed; result exceeds the 512 KiB program limit. Read the saved output in smaller pages; do not repeat the operation."
				result.Data = nil
				result.DataTruncated = true
				result.DisplayTruncated = true
				result.DataError = "program result exceeds 512 KiB"
			}
		}
		if result.DataError != "" && !journal.program {
			result.Output = "Tool execution completed, but its structured data is invalid: " + result.DataError + "\n" + result.Output
		}
		if journal.program && !failed && result.ObserveOutput != nil {
			result.ObserveOutput(result.Output)
			result.ObserveOutput = nil
		}
		return result
	}
	a.services.evbus.Emit(events.TopicToolPreExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "running"})
	started := toolEvent(tc, "running", "")
	started.Kind = protocol.EventToolStarted
	a.emit(started)
	a.emit(toolEvent(tc, "running", ""))
	a.emitStatus("%s running…", tc.Name)
	start := time.Now()
	tctx := a.buildToolContext(tc, turnCtx, cfg, sb, resources, skills)
	tctx.ObserveRead = func(path string, version fsops.Version) {
		observeRead = func(delivered string) {
			// At least the first content rune (or the empty-file marker) must
			// survive projection. A footer or a line number alone is not a read.
			text := execution.result.Text
			required := text
			if tab := strings.IndexByte(text, '\t'); tab >= 0 {
				_, size := utf8.DecodeRuneInString(text[tab+1:])
				required = text[:tab+1+size]
			}
			if resources != nil && required != "" && strings.HasPrefix(delivered, required) {
				resources.Files.MarkVersion(path, version)
			}
		}
	}
	a.bindProcessEvents(tctx, tc, journal)
	a.bindProjectMemoryInvocation(tctx, cfg, journal.turnID)
	if journal.program {
		tctx.OutputLimit = codemode.MaxResult
	}
	tctx.RunCode = func(code string, timeout time.Duration) (protocol.CodeResult, error) {
		return a.runCode(turnCtx, tc, journal, code, timeout)
	}
	if err := a.toolExecutionError(); err != nil {
		execution.setStatus("cancelled")
		return finishToolOutput(err.Error(), true)
	}
	var out string
	var err error
	if turnCtx != nil {
		err = turnCtx.Err()
	}
	if err == nil {
		if journal.program {
			var unknown bool
			execution.result, err, unknown = executeProgramTool(tool, tctx)
			if unknown {
				execution.setStatus("unknown")
				return finishToolOutput(err.Error(), true)
			}
		} else {
			execution.result, err = tool.Execute(tctx, tctx.Args)
		}
		out = execution.result.Text
		execution.failure = execution.result.Failure
		codeResult = execution.result.Code
		if codeResult != nil {
			for _, p := range codeResult.Completed {
				execution.result.CompletionIDs = append(execution.result.CompletionIDs, p.ID)
			}
		}
		if err == nil {
			if dataErr := journal.toolLease.ValidateOutput(tc.Name, execution.result.Data); dataErr != nil && execution.result.DataError == "" {
				execution.result.DataError = dataErr.Error()
			}
			if journal.program && execution.result.DataError == "" {
				if dataErr := codemode.ValidateData(execution.result.Data); dataErr != nil {
					execution.result.DataError = dataErr.Error()
				}
			}
		}
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	status := "ok"
	if err != nil {
		var unknown *processExecution.OutcomeUnknownError
		if errors.As(err, &unknown) || execution.failure != nil && execution.failure.SideEffect == "unknown" {
			execution.setStatus("unknown")
		} else if errors.Is(err, processExecution.ErrEnvironmentUnavailable) {
			execution.failure = &protocol.ToolFailure{Code: "environment_unavailable", DecisionSource: "environment", ExecutionStarted: "no", RetryClass: "after_environment_change"}
		} else if turnCtx != nil && turnCtx.Err() != nil {
			execution.setStatus("cancelled")
		}
		status = err.Error()
	}
	a.logv("tool %s %s in %s", tc.Name, status, elapsed)

	// Invalidate the workspace model after anything that may have changed files.
	switch invocationKind(tool, tc.Arguments) {
	case permissions.KindWrite, permissions.KindShell, permissions.KindProcess, permissions.KindProcessStop, permissions.KindUnknown, permissions.KindComputer:
		workspace.Invalidate(cfg.Workspace)
	}

	if err != nil {
		raw := fmt.Sprintf("%s failed after %s: %v", tc.Name, elapsed, err)
		if strings.TrimSpace(out) != "" {
			raw += "\n" + out
		}
		result := finishToolOutput(raw, true)
		a.emit(toolEvent(tc, "error", result.Output))
		a.services.evbus.Emit(events.TopicToolPostExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "error", Error: err.Error()})
		// PostToolUseFailure hooks observe the failure (non-blocking).
		a.runHookWithJournal(turnCtx, hooks.EventPostToolUseFailure, func(hctx context.Context) hooks.Output {
			return a.services.hooks.PostToolUseFailure(hctx, tc.Name, tc.Arguments, err.Error())
		})
		return result
	}

	// PostToolUse shell hooks can append context to the result.
	if ho := a.runHookWithJournal(turnCtx, hooks.EventPostToolUse, func(hctx context.Context) hooks.Output {
		return a.services.hooks.PostToolUse(hctx, tc.Name, tc.Arguments, out)
	}); ho.HookSpecificOutput != "" {
		if codeResult != nil {
			codeResult.Note = strings.TrimSpace(codeResult.Note + "\n" + ho.HookSpecificOutput)
		} else {
			out += "\n" + ho.HookSpecificOutput
		}
	}
	// In-process Go post hooks run alongside.
	if extra := a.services.gohooks.RunPostTool(tc.Name, tc.Arguments, out); extra != "" {
		if codeResult != nil {
			codeResult.Note = strings.TrimSpace(codeResult.Note + "\n" + extra)
		} else {
			out += extra
		}
	}
	result := finishToolOutput(out, false)
	if result.IsError {
		a.emit(toolEvent(tc, "error", result.Output))
		return result
	}

	a.emit(toolEvent(tc, "success", result.Output))
	a.services.evbus.Emit(events.TopicToolPostExecute, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "success", Output: result.Output})
	a.services.evbus.Emit(events.TopicToolResult, events.ToolEvent{ToolName: tc.Name, Args: tc.Arguments, Status: "success", Output: result.Output})
	return result
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
	tctx.Args = make(map[string]any, len(tc.Arguments))
	for name, value := range tc.Arguments {
		if name == "requested_capabilities" {
			continue
		}
		tctx.Args[name] = value
	}
	tctx.HostNetworkAllowed = sb != nil && sb.NetworkAllowed()
	if cfg.ComputerUseEnabled && a.children.childState == nil && resources != nil {
		tctx.Computer = resources.Computer()
	}
	tctx.Timeout = cfg.BashTimeout()
	tctx.ReplaceTodos = a.replaceTodos
	tctx.ReadOutput = a.readOutput
	if cfg.MaxResultSizeBytes > 0 {
		tctx.OutputLimit = cfg.MaxResultSizeBytes
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
	return tctx
}
