package agent

// This file is the execution-side adapter for the typed tool journal. The
// compatibility Agent event stream remains useful for old callers, but the
// session log is the authority for whether a tool was admitted, completed,
// and projected into the next model request.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

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

// decodeProviderToolCalls is intentionally strict. UnmarshalArgs historically
// converted malformed, null, and array arguments into {}, which could turn a
// corrupt model response into a real default-argument side effect. Preserve
// JSON numbers with UseNumber so permission fingerprints and typed journal
// facts use the same value the provider returned.
func decodeProviderToolCalls(raw []llm.ToolCall) ([]messages.ToolCall, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	calls := make([]messages.ToolCall, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for i, call := range raw {
		if strings.TrimSpace(call.ID) == "" {
			return nil, fmt.Errorf("tool call %d has an empty id", i)
		}
		if seen[call.ID] {
			return nil, fmt.Errorf("tool call %q is duplicated", call.ID)
		}
		seen[call.ID] = true
		if strings.TrimSpace(call.Function.Name) == "" {
			return nil, fmt.Errorf("tool call %q has no tool name", call.ID)
		}
		rawArgs := strings.TrimSpace(call.Function.Arguments.String())
		if rawArgs == "" || rawArgs == "null" {
			return nil, fmt.Errorf("tool call %q has null or empty arguments", call.ID)
		}
		decoder := json.NewDecoder(strings.NewReader(rawArgs))
		decoder.UseNumber()
		var args map[string]any
		if err := decoder.Decode(&args); err != nil || args == nil {
			if err == nil {
				err = errors.New("arguments must be a JSON object")
			}
			return nil, fmt.Errorf("tool call %q arguments must be an object: %w", call.ID, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("tool call %q arguments contain trailing JSON", call.ID)
			}
			return nil, fmt.Errorf("tool call %q arguments are malformed: %w", call.ID, err)
		}
		calls = append(calls, messages.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: args})
	}
	return calls, nil
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
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return fail(err)
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: stableID("tool-started", struct {
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
	artifacts, err := p.requestArtifacts()
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
	resultText := boundedToolFactText(output, toolFactPreviewBytes)
	effectID := stableID("tool-effect", struct {
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
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return fail(err)
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: stableID("tool-finished", struct {
			TurnID string
			StepID string
			CallID string
		}{j.ctx.turnID, j.ctx.stepID, tc.ID}),
		Events: []session.Event{event},
	}); err != nil {
		return fail(fmt.Errorf("agent: persist ToolFinished: %w", err))
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

func boundedToolFactText(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	return truncateUTF8Bytes(value, maxBytes)
}

func truncateUTF8Bytes(value string, maxBytes int) string {
	if maxBytes < 0 || len(value) <= maxBytes {
		return value
	}
	if maxBytes == 0 {
		return ""
	}
	if maxBytes >= len(value) {
		return value
	}
	cut := value[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		_, size := utf8.DecodeLastRuneInString(cut)
		if size <= 0 || size > len(cut) {
			cut = cut[:len(cut)-1]
		} else {
			cut = cut[:len(cut)-size]
		}
	}
	return cut
}

func (a *Agent) toolOutputPreview(value string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = a.cfg.MaxResultSizeChars
	}
	if maxBytes <= 0 {
		maxBytes = toolJournalMaxText
	}
	return truncateUTF8Bytes(value, maxBytes)
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
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: stableID("tool-projected", struct{ TurnID, StepID string }{jc.turnID, jc.stepID}),
		Events:        []session.Event{session.ToolResultsProjected{TurnID: jc.turnID, StepID: jc.stepID, Results: projected, Budget: budget}},
	}); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist ToolResultsProjected: %w", err))
	}
	return nil
}

func (a *Agent) persistTasksSnapshot() error {
	a.mu.Lock()
	p := a.persistence
	resources := a.resources
	a.mu.Unlock()
	if p == nil || resources == nil || resources.Todos == nil {
		return errors.New("agent: task persistence resources are unavailable")
	}
	states := resources.Todos.Snapshot()
	tasks := make([]session.Task, 0, len(states))
	for _, state := range states {
		tasks = append(tasks, session.Task{TaskID: state.ID, Title: state.Content, Status: state.Status})
	}
	if err := p.persistTasks(tasks); err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist task state: %w", err))
	}
	return nil
}

func (a *Agent) persistToolDiscovery(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("agent: discovered tool name is required")
	}
	a.mu.Lock()
	p := a.persistence
	registry := a.registry
	version := a.catalogVersion
	a.mu.Unlock()
	if p == nil || registry == nil {
		return errors.New("agent: tool discovery persistence is unavailable")
	}
	tool, ok := registry.Get(name)
	if !ok || tool == nil {
		return fmt.Errorf("agent: discovered tool %q is no longer registered", name)
	}
	schema, err := json.Marshal(tool.Parameters())
	if err != nil {
		return fmt.Errorf("agent: encode discovered tool %q schema: %w", name, err)
	}
	if version == 0 {
		version = 1
	}
	return p.persistToolsDiscovered(version, []session.ToolSchema{{ToolID: name, Version: fmt.Sprintf("catalog-%d", version), Description: tool.Description(), Schema: schema}})
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
	} else if a != nil && a.registry != nil {
		// The zero-context compatibility path may be used by package tests and
		// old embedders. Resolve the implementation before classifying it so a
		// custom tool that merely steals a built-in name cannot be treated as
		// read-only.
		tool, _ = a.registry.Get(call.Name)
	} else {
		return false
	}
	return readOnlyImplementation(tool)
}

// projectToolResults applies the aggregate budget in original call order.
// The returned values are exactly the strings appended to history, so the
// event and live projection cannot disagree when workers finish out of order.
func (a *Agent) projectToolResults(jc toolJournalContext, calls []messages.ToolCall, results []toolRunResult) (session.Budget, error) {
	budget := session.Budget{}
	if jc.maxOutputChars > 0 {
		budget.Limit = int64(jc.maxOutputChars)
	}
	a.mu.Lock()
	budget.Used = int64(a.outputBudget)
	a.mu.Unlock()
	for i := range results {
		out := results[i].output
		if jc.maxOutputChars > 0 {
			// Reserve a small, deterministic envelope for each result's call
			// identity/status.  The envelope is counted in Budget.Used even
			// though it is represented by typed fields in the event, so a
			// zero/very-small budget cannot accidentally become unlimited and
			// later results cannot consume bytes needed to identify this call.
			reserve := int64(32 + len(calls[i].ID))
			remaining := budget.Limit - budget.Used
			if remaining < 0 {
				remaining = 0
			}
			outputLimit := remaining - reserve
			if outputLimit < 0 {
				outputLimit = 0
			}
			if int64(len(out)) > outputLimit {
				budget.Truncated = true
				out = truncateUTF8Bytes(out, int(outputLimit))
			}
			budget.Used += reserve + int64(len(out))
			if budget.Used > budget.Limit {
				// A cap smaller than the minimum envelope is still a hard cap;
				// do not report a usage value that exceeds it.
				budget.Used = budget.Limit
			}
		} else {
			budget.Used += int64(len(out))
		}
		results[i].output = out
	}
	a.mu.Lock()
	a.outputBudget = int(budget.Used)
	a.mu.Unlock()
	if !jc.enabled {
		return budget, nil
	}
	if err := a.persistToolResultsProjected(jc, calls, results, budget); err != nil {
		return budget, err
	}
	return budget, nil
}

func (a *Agent) persistTurnStarted(turnID, stepID, inputID string) error {
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(errors.New("agent: session persistence is unavailable"))
	}
	a.mu.Lock()
	settingsRev, contextRev := a.settingsRev, a.contextRev
	a.mu.Unlock()
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
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
	event := session.TurnFinished{TurnID: turnID, Outcome: outcome, Error: boundedToolFactText(finishErr, toolFactPreviewBytes), DurationMs: durationMs}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
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
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
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
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
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
	sessionID, turnID, workspace, policyRevision, stepID := a.sessionID, fmt.Sprintf("turn-%d", a.turnSeq), a.cfg.Workspace, a.settingsRev, a.stepSeq
	a.mu.Unlock()
	return session.ApprovalRequest{
		ApprovalID: stableID("approval", struct {
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
