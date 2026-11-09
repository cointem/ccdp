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

	"ccdp/internal/config"
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

// toolJournalContext is frozen at the model-step boundary. Every dispatched
// call records its execution facts, including calls inside Code Mode.
type toolJournalContext struct {
	turnID         string
	stepID         string
	callIndex      int
	policyRevision uint64
	workspace      string
	maxParallel    int
	maxOutputBytes int
	toolLease      *tools.Lease
	context        context.Context
	config         *config.Config
	allowedTools   []string
	parentCallID   string
	childIndex     int
	program        bool
}

// releaseStepLease closes the MCP generation reference (and its frozen tool
// view) at the end of one model step.  It must be called by the turn loop on
// every step exit; holding a lease until TurnFinished would retain stale MCP
// clients across fallback/continuation requests and delay shutdown.
func releaseStepLease(step stepRuntime) {
	if step.mcpLease != nil {
		step.mcpLease.Close()
	}
}

func toolJournalContextForStep(step stepRuntime) toolJournalContext {
	names := make([]string, 0, len(step.tools))
	for _, t := range step.tools {
		names = append(names, t.Function.Name)
	}
	cfg := cloneConfig(&step.cfg)
	return toolJournalContext{
		config: &cfg, allowedTools: names,
		turnID:         fmt.Sprintf("turn-%d", step.turn),
		stepID:         fmt.Sprintf("step-%d", step.step),
		policyRevision: step.settingsRev,
		workspace:      step.cfg.Workspace,
		maxParallel:    step.cfg.MaxParallelTools,
		maxOutputBytes: step.cfg.MaxToolOutputBytesPerTurn,
		toolLease:      step.toolLease,
	}
}

type toolJournalExecution struct {
	failure          *protocol.ToolFailure
	ctx              toolJournalContext
	started          bool
	finished         bool
	status           string
	result           tools.Result
	displayTruncated bool
	outputID         string
}

func (j *toolJournalExecution) setStatus(status string) {
	if status == "" {
		return
	}
	j.status = status
}

func (j *toolJournalExecution) start(a *Agent, tc messages.ToolCall) error {
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
	processID := ""
	if tc.Name == "Process" {
		processID, _ = tc.Arguments["id"].(string)
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: messages.StableID("tool-started", struct {
			TurnID string
			StepID string
			CallID string
		}{j.ctx.turnID, j.ctx.stepID, tc.ID}),
		Events: []session.Event{session.ToolStarted{ProcessID: processID,
			TurnID: j.ctx.turnID, StepID: j.ctx.stepID, CallIndex: j.ctx.callIndex,
			ParentCallID: j.ctx.parentCallID, ChildIndex: j.ctx.childIndex, ResultTarget: j.ctx.resultTarget(),
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
	if j.finished {
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
	full := output
	if j.result.FullText != "" {
		full = j.result.FullText
	}
	var ref session.BlobRef
	if process := j.result.Process; process != nil && process.Terminal() && process.OutputID != "" {
		// The completion already saved the full log before wait returned.
		ref, _, err = p.OutputArtifact(process.OutputID)
	} else {
		ref, err = artifacts.PutContext(context.Background(), []byte(full))
	}
	if err != nil {
		return fail(fmt.Errorf("agent: persist raw tool output: %w", err))
	}
	ref.MediaType = "text/plain; charset=utf-8"
	j.outputID = ref.Hash
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
	if isErr && j.failure == nil {
		code, source, started, retry := "tool_failed", "tool", "unknown", "inspect_result"
		if !j.started {
			started = "no"
		}
		switch status {
		case "unknown":
			code, source, started, retry = "outcome_unknown", "executor", "unknown", "never_replay"
		case "denied":
			code, source, retry = "policy_denied", "policy", "after_policy_change"
		case "cancelled":
			code, source, retry = "cancelled", "runtime", "inspect_result"
		case "resource_limit":
			code, source, retry = "resource_limit", "runtime", "after_resource_change"
		}
		j.failure = &protocol.ToolFailure{Code: code, DecisionSource: source, ExecutionStarted: started, RetryClass: retry}
	}
	sideEffect := "none"
	if j.started {
		sideEffect = "attempted"
		if !isErr {
			sideEffect = "completed"
		}
	}
	if j.failure != nil && j.failure.SideEffect != "" {
		sideEffect = j.failure.SideEffect
	}
	resultText := tooling.BoundedFactText(output, toolFactPreviewBytes)
	effectID := messages.StableID("tool-effect", struct {
		TurnID string
		StepID string
		CallID string
	}{j.ctx.turnID, j.ctx.stepID, tc.ID})
	event := session.ToolFinished{
		Failure: j.failure,
		TurnID:  j.ctx.turnID, StepID: j.ctx.stepID, CallID: tc.ID,
		ParentCallID: j.ctx.parentCallID, ChildIndex: j.ctx.childIndex, ResultTarget: j.ctx.resultTarget(),
		EffectID: effectID, Status: status, SideEffect: sideEffect,
		RawOutput: &ref,
		Result:    session.ToolResult{CallID: tc.ID, Status: status, Text: resultText},
	}
	event.Process = protocol.CloneProcessOutput(j.result.Process)
	if image := j.result.Image; image != nil {
		imageRef, e := p.PutBlob(context.Background(), image.Data, image.MediaType)
		if e != nil {
			return fail(fmt.Errorf("persist tool image: %w", e))
		}
		event.Image = imageRef
	}
	event.CompletionIDs = append([]string(nil), j.result.CompletionIDs...)
	event.DataError = j.result.DataError
	event.DisplayTruncated = j.displayTruncated || resultText != output
	event.DataTruncated = j.result.DataTruncated
	event.Result.DataError = j.result.DataError
	event.Result.DisplayTruncated = event.DisplayTruncated
	if len(j.result.Data) > 0 {
		dataRef, e := artifacts.PutContext(context.Background(), j.result.Data)
		if e != nil {
			return fail(fmt.Errorf("persist tool data: %w", e))
		}
		dataRef.MediaType = "application/json"
		event.Data = &dataRef
	}
	if j.ctx.program {
		args, _ := json.Marshal(tc.Arguments)
		event.Call = &session.ToolCall{CallID: tc.ID, ToolID: tc.Name, Arguments: args}
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

func (j toolJournalContext) resultTarget() string {
	if j.program {
		return "program"
	}
	return ""
}

func (a *Agent) toolJournalFailure(err error) error {
	if err == nil {
		return nil
	}
	a.markPersistenceFailure(err)
	return err
}

func (a *Agent) toolOutputPreview(value string, maxBytes int, outputID ...string) string {
	if maxBytes <= 0 {
		a.mu.Lock()
		maxBytes = a.services.cfg.MaxResultSizeBytes
		a.mu.Unlock()
	}
	if maxBytes <= 0 {
		maxBytes = toolJournalMaxText
	}
	if len(value) <= maxBytes {
		return value
	}
	marker := "\n[incomplete: tool output limit; use ReadOutput for the saved output]"
	if len(outputID) > 0 && outputID[0] != "" {
		marker = "\n[incomplete: use ReadOutput with output_id=" + outputID[0] + "]"
	}
	if maxBytes < len(marker) {
		return messages.TruncateUTF8(value, maxBytes)
	}
	return messages.TruncateUTF8(value, maxBytes-len(marker)) + marker
}

// finishUnexecutedToolWithStatus records a rejected/cancelled call that never crossed
// the ToolStarted side-effect boundary. It is still a durable result for the
// assistant tool call and is never eligible for automatic rerun.
func (a *Agent) finishUnexecutedToolWithStatus(jc toolJournalContext, tc messages.ToolCall, output string, isErr bool, status string) error {
	j := &toolJournalExecution{ctx: jc, status: status}
	return j.finish(a, tc, output, isErr)
}

// persistToolResultsProjected commits the deterministic, model-visible
// projection after every raw completion in this step has been admitted.
func (a *Agent) persistToolResultsProjected(jc toolJournalContext, calls []messages.ToolCall, results []tooling.Result, budget session.Budget) error {
	if len(calls) == 0 || len(calls) != len(results) {
		return a.toolJournalFailure(errors.New("agent: tool projection call/result count mismatch"))
	}
	projected := make([]session.ProjectedToolResult, 0, len(results))
	for i, result := range results {
		status := result.Status
		if status == "" {
			if result.IsError {
				status = "error"
			} else {
				status = "success"
			}
		}
		projected = append(projected, session.ProjectedToolResult{CallID: calls[i].ID, Status: status, Text: result.Output, DisplayTruncated: result.DisplayTruncated, DataError: result.DataError})
	}
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	ids := projectedProcessIDs(results)
	undelivered := ids[:0]
	a.mu.Lock()
	for _, id := range ids {
		if proc, ok := a.conversation.durable.Processes[id]; ok && proc.Result != nil && !proc.Projected {
			undelivered = append(undelivered, id)
		}
	}
	a.mu.Unlock()
	events := []session.Event{session.ToolResultsProjected{TurnID: jc.turnID, StepID: jc.stepID, Results: projected, Budget: budget}}
	if len(undelivered) > 0 {
		links := make(map[string]string, len(undelivered))
		for i, r := range results {
			for _, id := range r.CompletionIDs {
				if _, linked := links[id]; !linked {
					links[id] = calls[i].ID
				}
			}
		}
		for id := range links {
			found := false
			for _, kept := range undelivered {
				found = found || id == kept
			}
			if !found {
				delete(links, id)
			}
		}
		events = append(events, session.ProcessResultsProjected{TurnID: jc.turnID, StepID: jc.stepID, ToolCalls: links, IDs: undelivered, Budget: budget})
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: messages.StableID("tool-projected", struct{ TurnID, StepID string }{jc.turnID, jc.stepID}),
		Events:        events,
	}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist ToolResultsProjected: %w", err))
	}
	return nil
}

func (a *Agent) replaceTodos(states []tools.TodoState) error {
	tasks := make([]session.Task, 0, len(states))
	for _, state := range states {
		tasks = append(tasks, session.Task{TaskID: state.ID, Title: state.Content, Status: state.Status, Priority: state.Priority})
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
	tool, _ := jc.toolLease.Get(call.Name)
	return invocationKind(tool, call.Arguments).Concurrent()
}

// projectToolResults applies the aggregate budget in original call order.
// The returned values are exactly the strings appended to history, so the
// event and live projection cannot disagree when workers finish out of order.
func (a *Agent) projectToolResults(jc toolJournalContext, calls []messages.ToolCall, results []tooling.Result) (session.Budget, error) {
	projected := a.projectToolsFromWorker(jc, calls, results)
	copy(results, projected.results)
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
