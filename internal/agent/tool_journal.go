package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tooling"
	"ccdp/internal/tools"
)

// This file is the execution-side adapter for the typed tool journal. The
// compatibility Agent event stream remains useful for old callers, but the
// session log is the authority for whether a tool was admitted, completed,
// and projected into the next model request.

const (
	// Keep the event inline preview comfortably below the JSONL transaction
	// bound. The complete output is always retained in RawOutput.
	toolFactPreviewBytes = 64 << 10
	toolJournalMaxText   = 64 << 10
)

// toolJournalContext is frozen at the model-step boundary. A zero context is
// the legacy/direct-call compatibility path and deliberately does not write
// typed tool facts.
type toolJournalContext struct {
	enabled        bool
	turnID         string
	stepID         string
	callIndex      int
	policyRevision uint64
	workspace      string
	maxParallel    int
	maxOutputChars int
	toolLease      *tools.Lease
	statusSink     *string // per-call outcome written by the execution adapter
}

// releaseStepLease closes the MCP generation reference (and its frozen tool
// view) at the end of one model step.  It must be called by the turn loop on
// every step exit; holding a lease until TurnFinished would retain stale MCP
// clients across fallback/continuation requests and delay shutdown.
func releaseStepLease(step stepRuntime) {
	if step.mcpLease != nil {
		step.mcpLease.Close()
		return
	}
	if step.toolLease != nil {
		step.toolLease.Close()
	}
}

func toolJournalContextForStep(step stepRuntime) toolJournalContext {
	return toolJournalContext{
		enabled:        true,
		turnID:         fmt.Sprintf("turn-%d", step.turn),
		stepID:         fmt.Sprintf("step-%d", step.step),
		policyRevision: step.settingsRev,
		workspace:      step.cfg.Workspace,
		maxParallel:    step.cfg.MaxParallelTools,
		maxOutputChars: step.cfg.MaxToolOutputCharsPerTurn,
		toolLease:      step.toolLease,
	}
}

type toolJournalExecution struct {
	ctx      toolJournalContext
	started  bool
	finished bool
	status   string
}

func (j *toolJournalExecution) setStatus(status string) {
	if j == nil || status == "" {
		return
	}
	j.status = status
	if j.ctx.statusSink != nil {
		*j.ctx.statusSink = status
	}
}

func (j *toolJournalExecution) start(a *Agent, tc messages.ToolCall) error {
	if j == nil || !j.ctx.enabled {
		return nil
	}
	if j.started {
		return nil
	}
	fail := func(err error) error {
		j.setStatus("error")
		return a.toolJournalFailure(err)
	}
	if strings.TrimSpace(j.ctx.turnID) == "" || strings.TrimSpace(j.ctx.stepID) == "" || strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.Name) == "" {
		return fail(errors.New("agent: tool journal start identity is required"))
	}
	args, err := json.Marshal(tc.Arguments)
	if err != nil {
		return fail(fmt.Errorf("agent: encode tool arguments: %w", err))
	}
	if !json.Valid(args) {
		return fail(errors.New("agent: tool arguments are not valid JSON"))
	}
	sum := sha256.Sum256(args)
	fingerprint := hex.EncodeToString(sum[:])
	p := a.persistenceHandle()
	if p == nil {
		return fail(errors.New("agent: session persistence is unavailable"))
	}
	if err := a.persistenceFailure(); err != nil {
		return fail(err)
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: messages.StableID("tool-started", struct {
			TurnID string
			StepID string
			CallID string
		}{j.ctx.turnID, j.ctx.stepID, tc.ID}),
		Events: []session.Event{session.ToolStarted{
			TurnID: j.ctx.turnID, StepID: j.ctx.stepID, CallIndex: j.ctx.callIndex,
			Call:        session.ToolCall{CallID: tc.ID, ToolID: tc.Name, Arguments: append([]byte(nil), args...)},
			Fingerprint: fingerprint, PolicyRevision: j.ctx.policyRevision,
		}},
	}); err != nil {
		return fail(fmt.Errorf("agent: persist ToolStarted: %w", err))
	}
	j.started = true
	return nil
}

func (j *toolJournalExecution) finish(a *Agent, tc messages.ToolCall, output string, isErr bool) error {
	if j == nil || !j.ctx.enabled || j.finished {
		return nil
	}
	fail := func(err error) error {
		j.setStatus("error")
		return a.toolJournalFailure(err)
	}
	if strings.TrimSpace(j.ctx.turnID) == "" || strings.TrimSpace(j.ctx.stepID) == "" || strings.TrimSpace(tc.ID) == "" {
		return fail(errors.New("agent: tool journal finish identity is required"))
	}
	p := a.persistenceHandle()
	if p == nil {
		return fail(errors.New("agent: session persistence is unavailable"))
	}
	// The tool's context is allowed to be cancelled after it returns. The
	// durable completion is a recovery fact, so artifact publication uses a
	// non-cancellable context; otherwise an ordinary interrupt would poison a
	// session after the tool's outcome was already known.
	artifacts, err := p.RequestArtifacts()
	if err != nil {
		return fail(err)
	}
	ref, err := artifacts.PutContext(context.Background(), []byte(output))
	if err != nil {
		return fail(fmt.Errorf("agent: persist raw tool output: %w", err))
	}
	ref.MediaType = "text/plain; charset=utf-8"
	status := j.status
	if status == "" {
		if isErr {
			if j.started {
				status = "error"
			} else {
				status = "denied"
			}
		} else {
			status = "success"
		}
	}
	j.setStatus(status)
	sideEffect := "none"
	if j.started {
		sideEffect = "attempted"
		if !isErr {
			sideEffect = "completed"
		}
	}
	resultText := tooling.BoundedFactText(output, toolFactPreviewBytes)
	effectID := messages.StableID("tool-effect", struct {
		TurnID string
		StepID string
		CallID string
	}{j.ctx.turnID, j.ctx.stepID, tc.ID})
	event := session.ToolFinished{
		TurnID: j.ctx.turnID, StepID: j.ctx.stepID, CallID: tc.ID,
		EffectID: effectID, Status: status, SideEffect: sideEffect,
		RawOutput: &ref,
		Result:    session.ToolResult{CallID: tc.ID, Status: status, Text: resultText},
	}
	if isErr {
		event.Error = resultText
	}
	if err := a.settleToolResult(event); err != nil {
		return fail(err)
	}
	j.finished = true
	return nil
}

func (a *Agent) toolJournalFailure(err error) error {
	if err == nil {
		return nil
	}
	a.markPersistenceFailure(err)
	return err
}

func (a *Agent) toolOutputPreview(value string, maxBytes int) string {
	if maxBytes <= 0 {
		a.mu.Lock()
		maxBytes = a.services.cfg.MaxResultSizeChars
		a.mu.Unlock()
	}
	if maxBytes <= 0 {
		maxBytes = toolJournalMaxText
	}
	if len(value) <= maxBytes {
		return value
	}
	marker := "[tool result exceeds output limit; request a smaller result]"
	if json.Valid([]byte(value)) {
		if len(marker) > maxBytes {
			return ""
		}
		return marker
	}
	marker = "\n[incomplete: tool output limit]"
	if maxBytes < len(marker) {
		return ""
	}
	return messages.TruncateUTF8(value, maxBytes-len(marker)) + marker
}

// finishUnexecutedTool records a rejected/cancelled call that never crossed
// the ToolStarted side-effect boundary. It is still a durable result for the
// assistant tool call and is never eligible for automatic rerun.
func (a *Agent) finishUnexecutedTool(jc toolJournalContext, tc messages.ToolCall, output string, isErr bool) error {
	return a.finishUnexecutedToolWithStatus(jc, tc, output, isErr, "denied")
}

func (a *Agent) finishUnexecutedToolWithStatus(jc toolJournalContext, tc messages.ToolCall, output string, isErr bool, status string) error {
	if !jc.enabled {
		return nil
	}
	j := &toolJournalExecution{ctx: jc, status: status}
	return j.finish(a, tc, output, isErr)
}

// persistToolResultsProjected commits the deterministic, model-visible
// projection after every raw completion in this step has been admitted.
func (a *Agent) persistToolResultsProjected(jc toolJournalContext, calls []messages.ToolCall, results []toolRunResult, budget session.Budget) error {
	if !jc.enabled {
		return nil
	}
	if len(calls) == 0 || len(calls) != len(results) {
		return a.toolJournalFailure(errors.New("agent: tool projection call/result count mismatch"))
	}
	projected := make([]session.ProjectedToolResult, 0, len(results))
	for i, result := range results {
		status := result.status
		if status == "" {
			if result.isErr {
				status = "error"
			} else {
				status = "success"
			}
		}
		projected = append(projected, session.ProjectedToolResult{CallID: calls[i].ID, Status: status, Text: result.output})
	}
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: messages.StableID("tool-projected", struct{ TurnID, StepID string }{jc.turnID, jc.stepID}),
		Events:        []session.Event{session.ToolResultsProjected{TurnID: jc.turnID, StepID: jc.stepID, Results: projected, Budget: budget}},
	}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist ToolResultsProjected: %w", err))
	}
	return nil
}

func (a *Agent) persistTasksSnapshot() error {
	a.mu.Lock()
	p := a.services.persistence
	resources := a.services.resources
	a.mu.Unlock()
	if p == nil || resources == nil || resources.Todos == nil {
		return errors.New("agent: task persistence resources are unavailable")
	}
	states, err := resources.Todos.Snapshot()
	if err != nil {
		return err
	}
	tasks := make([]session.Task, 0, len(states))
	for _, state := range states {
		tasks = append(tasks, session.Task{TaskID: state.ID, Title: state.Content, Status: state.Status})
	}
	if err := a.saveTasksFromWorker(tasks); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist task state: %w", err))
	}
	return nil
}

func (a *Agent) persistToolDiscovery(version uint64, name, description string, parameters map[string]any) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("agent: discovered tool name is required")
	}
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: tool discovery persistence is unavailable")
	}
	schema, err := json.Marshal(parameters)
	if err != nil {
		return fmt.Errorf("agent: encode discovered tool %q schema: %w", name, err)
	}
	if version == 0 {
		version = 1
	}
	return a.discoverToolsFromWorker(version, []session.ToolSchema{{ToolID: name, Version: fmt.Sprintf("catalog-%d", version), Description: description, Schema: schema}})
}

// toolCallIsReadOnly is deliberately conservative.  Only built-ins whose
// registered effect is exactly read may share a dispatch pool.  Network,
// process, delegate, plan, and unknown/custom tools are barriers: even when a
// particular invocation happens to be harmless, the scheduler must not infer
// that from a name it has not explicitly classified.  A barrier also gives
// workspace/resource-owning tools an ordered happens-before edge.
func (a *Agent) toolCallIsReadOnly(jc toolJournalContext, call messages.ToolCall) bool {
	var tool tools.Tool
	if jc.toolLease != nil {
		tool, _ = jc.toolLease.Get(call.Name)
	} else if a != nil && a.services.registry != nil {
		// The zero-context compatibility path may be used by package tests and
		// old embedders. Resolve the implementation before classifying it so a
		// custom tool that merely steals a built-in name cannot be treated as
		// read-only.
		tool, _ = a.services.registry.Get(call.Name)
	} else {
		return false
	}
	return toolKind(tool).Concurrent()
}

// projectToolResults applies the aggregate budget in original call order.
// The returned values are exactly the strings appended to history, so the
// event and live projection cannot disagree when workers finish out of order.
func (a *Agent) projectToolResults(jc toolJournalContext, calls []messages.ToolCall, results []toolRunResult) (session.Budget, error) {
	projected := a.projectToolsFromWorker(jc, calls, results)
	for i, result := range projected.results {
		if result.output != results[i].output && calls[i].Name == "Read" {
			a.mu.Lock()
			resources, sb := a.services.resources, a.services.sandbox
			a.mu.Unlock()
			if resources != nil && sb != nil {
				if path, e := sb.ResolveRead(tools.StringArg(calls[i].Arguments, "file_path", "")); e == nil {
					resources.Files.ForgetFile(path)
				}
			}
		}
		results[i] = result
	}
	return projected.budget, projected.err
}

func (a *Agent) persistTurnStarted(turnID, stepID, inputID string) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	a.mu.Lock()
	settingsRev, contextRev := a.sequences.settingsRev, a.sequences.contextRev
	a.mu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	event := session.TurnStarted{TurnID: turnID, StepID: stepID, ContextRev: contextRev, SettingsRev: settingsRev}
	if inputID != "" {
		event.InputIDs = []string{inputID}
	}
	if _, err := p.Commit(session.Batch{TransactionID: "turn-started-" + turnID, Events: []session.Event{event}}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist TurnStarted: %w", err))
	}
	return nil
}

func (a *Agent) persistTurnFinished(turnID, outcome, finishErr string, duration time.Duration) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	durationMs := max(int64(1), duration.Milliseconds())
	event := session.TurnFinished{TurnID: turnID, Outcome: outcome, Error: tooling.BoundedFactText(finishErr, toolFactPreviewBytes), DurationMs: durationMs}
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	if _, err := p.Commit(session.Batch{TransactionID: "turn-finished-" + turnID, Events: []session.Event{event}}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist TurnFinished: %w", err))
	}
	return nil
}

func (a *Agent) persistApprovalRequested(request session.ApprovalRequest) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	if _, err := p.Commit(session.Batch{TransactionID: "approval-requested-" + request.ApprovalID, Events: []session.Event{session.ApprovalRequested{Request: request}}}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist ApprovalRequested: %w", err))
	}
	return nil
}

func (a *Agent) persistApprovalResolved(resolution session.ApprovalResolution) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	if _, err := p.Commit(session.Batch{TransactionID: "approval-resolved-" + resolution.ApprovalID, Events: []session.Event{session.ApprovalResolved{Resolution: resolution}}}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist ApprovalResolved: %w", err))
	}
	return nil
}

func toolApprovalRequest(a *Agent, tc messages.ToolCall, reason string, capabilities []protocol.CapabilityRequest) session.ApprovalRequest {
	args, _ := json.Marshal(tc.Arguments)
	sum := sha256.Sum256(args)
	businessArgs := make(map[string]any, len(tc.Arguments))
	for key, value := range tc.Arguments {
		if key != "requested_capabilities" {
			businessArgs[key] = value
		}
	}
	businessJSON, _ := json.Marshal(businessArgs)
	businessSum := sha256.Sum256(businessJSON)
	a.mu.Lock()
	sessionID, turnID, workspace, policyRevision, stepID := a.conversation.sessionID, fmt.Sprintf("turn-%d", a.sequences.turnSeq), a.services.cfg.Workspace, a.sequences.settingsRev, a.sequences.stepSeq
	a.mu.Unlock()
	return session.ApprovalRequest{
		ApprovalID: messages.StableID("approval", struct {
			SessionID string
			TurnID    string
			StepID    uint64
			CallID    string
		}{sessionID, turnID, stepID, tc.ID}),
		SessionID: sessionID, TurnID: turnID, CallID: tc.ID,
		ArgumentDigest: hex.EncodeToString(sum[:]), BusinessArgumentDigest: hex.EncodeToString(businessSum[:]), Workspace: workspace,
		ToolVersion: tc.Name, PolicyRevision: policyRevision,
		Capabilities: append([]protocol.CapabilityRequest(nil), capabilities...),
	}
}
