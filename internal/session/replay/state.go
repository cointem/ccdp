package replay

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type State struct {
	Processes        map[string]Process
	ProcessOrder     []string
	pendingProcesses []string
	ID               string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// TurnSeq/StepSeq are monotonic counters derived from durable fact IDs.
	// They are not a projection of usage or history; they only prevent a
	// resumed Agent from reusing a transaction identity already in the log.
	TurnSeq           uint64
	StepSeq           uint64
	Workspace         string
	Model             string
	Settings          session.Settings
	SettingsRev       uint64
	CommandDigests    map[string]string // retained after completion for retry identity
	ScheduledCommands map[string]session.CommandScheduled
	CompletedCommands map[string]session.CommandCompleted
	GoalAccountingIDs map[string]bool
	Goal              *protocol.Goal
	Tasks             []session.Task
	CatalogVersion    uint64
	History           []messages.Message
	outputs           map[string]session.BlobRef
	historyStep       []string // parallel step identity for typed assistant/tool facts
	Usage             session.Usage
	pendingOrder      []string
	pendingByID       map[string]messages.Message
	pendingInput      map[string]session.InputQueued
	startedTools      map[toolFactKey]session.ToolStarted
	startedOrder      []toolFactKey
	finishedTools     map[toolFactKey]session.ToolFinished
	finishedOrder     []toolFactKey
	projected         map[toolFactKey]bool
	ParentID          string
	BranchPoint       int
	BranchSummary     string
	Discovered        map[string]session.ToolSchema
}

func runtimeSequence(id, prefix string) uint64 {
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(id, prefix), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func (p *State) observeRuntimeSequence(turnID, stepID string) {
	if p == nil {
		return
	}
	if n := runtimeSequence(turnID, "turn-"); n > p.TurnSeq {
		p.TurnSeq = n
	}
	if n := runtimeSequence(stepID, "step-"); n > p.StepSeq {
		p.StepSeq = n
	}
}

type toolFactKey struct {
	TurnID string
	StepID string
	CallID string
}

func makeToolFactKey(turnID, stepID, callID string) toolFactKey {
	return toolFactKey{TurnID: turnID, StepID: stepID, CallID: callID}
}

func (p *State) resolveToolFactKey(turnID, stepID, callID string) toolFactKey {
	key := makeToolFactKey(turnID, stepID, callID)
	if stepID != "" {
		return key
	}
	// Old facts did not carry StepID. If exactly one matching step exists,
	// attach the completion/projection to it; otherwise retain the empty-step
	// key so ambiguous legacy data is never merged across calls.
	var match toolFactKey
	found := false
	for candidate := range p.startedTools {
		if candidate.TurnID != turnID || candidate.CallID != callID {
			continue
		}
		if found {
			return key
		}
		match, found = candidate, true
	}
	if found {
		return match
	}
	for candidate := range p.finishedTools {
		if candidate.TurnID != turnID || candidate.CallID != callID {
			continue
		}
		if found && candidate != match {
			return key
		}
		match, found = candidate, true
	}
	if found {
		return match
	}
	return key
}

func (p *State) PendingInputViews() []protocol.InputView {
	if p == nil || len(p.pendingOrder) == 0 {
		return nil
	}
	views := make([]protocol.InputView, 0, len(p.pendingOrder))
	for _, inputID := range p.pendingOrder {
		input, ok := p.pendingInput[inputID]
		if !ok || input.Text == "" {
			continue
		}
		strategy := protocol.InputStrategy(input.Strategy)
		if strategy != protocol.InputSteer && strategy != protocol.InputFollowup && strategy != protocol.InputMessage {
			strategy = protocol.InputFollowup
		}
		views = append(views, protocol.InputView{
			ID: protocol.InputID(input.InputID), Text: input.Text, Strategy: strategy, State: "queued", CreatedAt: input.CreatedAt,
		})
	}
	return views
}

func (p *State) PendingAttachments() map[string][]messages.ImageAttachment {
	if p == nil || len(p.pendingByID) == 0 {
		return nil
	}
	out := make(map[string][]messages.ImageAttachment)
	for inputID, message := range p.pendingByID {
		if len(message.ImageAttachments) == 0 {
			continue
		}
		out[inputID] = messages.CloneImageAttachments(message.ImageAttachments)
	}
	return out
}

func New() *State {
	return &State{
		Processes:   make(map[string]Process),
		pendingByID: make(map[string]messages.Message), pendingInput: make(map[string]session.InputQueued),
		startedTools: make(map[toolFactKey]session.ToolStarted), finishedTools: make(map[toolFactKey]session.ToolFinished), projected: make(map[toolFactKey]bool),
		CommandDigests: make(map[string]string), ScheduledCommands: make(map[string]session.CommandScheduled), CompletedCommands: make(map[string]session.CommandCompleted),
		Discovered: make(map[string]session.ToolSchema),
	}
}

func (p *State) PendingCommands() []session.CommandScheduled {
	if p == nil || len(p.ScheduledCommands) == 0 {
		return nil
	}
	ids := make([]string, 0, len(p.ScheduledCommands))
	for id := range p.ScheduledCommands {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]session.CommandScheduled, 0, len(ids))
	for _, id := range ids {
		out = append(out, p.ScheduledCommands[id])
	}
	return out
}

func Records(records []session.Record) (*State, error) {
	p, err := RecordsRaw(records)
	if err != nil {
		return nil, err
	}
	p.FinalizeRecovery()
	return p, nil
}

// RecordsRaw replays only durable facts. Synthetic recovery results are
// deliberately added by FinalizeRecovery on a read projection, never cached
// in sessionPersistence: a subsequent raw completion must be able to close a
// Started call without duplicating an "unknown" message.
func RecordsRaw(records []session.Record) (*State, error) {
	p := New()
	for _, record := range records {
		if err := p.Apply(record.Event); err != nil {
			return nil, fmt.Errorf("agent: replay record %d: %w", record.Seq, err)
		}
	}
	return p, nil
}

func (p *State) FinalizeRecovery() {
	if p == nil {
		return
	}
	// A started tool without a durable completion is deliberately projected as
	// unknown.  It is never automatically re-run by this layer.
	for _, key := range p.orderedStartedIDs() {
		started := p.startedTools[key]
		if started.ResultTarget == "program" {
			continue
		}
		if _, finished := p.finishedTools[key]; finished {
			continue
		}
		if p.projected[key] {
			// A projection without a raw completion cannot prove that the
			// side effect completed. Replace it with the honest unknown state
			// rather than claiming a successful result.
			p.removeProjectedToolResult(key)
		}
		if started.Call.CallID == "" {
			continue
		}
		unknown := messages.Message{Role: messages.RoleTool, ToolCallID: key.CallID, ToolStatus: "unknown",
			Content: "Error: tool result unknown (the tool may have executed before the session stopped); it will not be re-run automatically"}
		var children []string
		for childKey, child := range p.startedTools {
			if childKey.TurnID == key.TurnID && childKey.StepID == key.StepID && child.ParentCallID == key.CallID {
				status := "unknown"
				if finished, ok := p.finishedTools[childKey]; ok {
					status = finished.Status
				}
				children = append(children, childKey.CallID+"="+status)
			}
		}
		for childKey, child := range p.finishedTools {
			if _, started := p.startedTools[childKey]; !started && childKey.TurnID == key.TurnID && childKey.StepID == key.StepID && child.ParentCallID == key.CallID {
				children = append(children, childKey.CallID+"="+child.Status)
			}
		}
		if len(children) > 0 {
			sort.Strings(children)
			unknown.Content += "\nInternal calls (inspect the transcript before retrying): " + strings.Join(children, ", ")
		}
		unknown.ID = messages.StableID("unknown-tool-result", key)
		p.insertRecoveredToolResult(key, unknown)
	}
	// If a raw completion was durably admitted but the batch projection was
	// lost during a crash, recover a legal tool-result pair from the raw facts
	// without executing the tool again. Sort by the frozen call ordinal when
	// available; old logs retain their append order via rawFinishedOrder.
	var recoveredImages []toolFactKey
	for _, key := range p.orderedFinishedIDs() {
		if p.projected[key] {
			continue
		}
		finished, ok := p.finishedTools[key]
		if !ok {
			continue
		}
		if finished.ResultTarget == "program" {
			continue
		}
		result := finished.Result
		content := result.Text
		if len(finished.CompletionIDs) > 0 {
			// Raw tool output may be durable without having reached the model.
			// Recover a reference here; pending process facts deliver the payload.
			var refs []protocol.ProcessOutput
			for _, id := range finished.CompletionIDs {
				if proc, ok := p.Processes[id]; ok {
					refs = append(refs, proc.State.ProcessOutput.Reference())
				}
			}
			if len(refs) > 0 {
				data, _ := json.Marshal(struct {
					Results  []protocol.ProcessOutput `json:"process_results"`
					OutputID string                   `json:"output_id,omitempty"`
				}{Results: refs, OutputID: func() string {
					if finished.RawOutput != nil {
						return finished.RawOutput.Hash
					}
					return ""
				}()})
				content = string(data)
			}
		}
		if result.Status != "success" && result.Status != "ok" && !strings.HasPrefix(content, "Error: ") {
			content = "Error: " + content
		}
		message := messages.Message{Role: messages.RoleTool, ToolCallID: key.CallID, ToolStatus: result.Status, Content: content, OutputRef: outputReference(finished)}
		message.SourceOutputs = p.processOutputReferences(finished.CompletionIDs)
		message.DisplayTruncated = result.DisplayTruncated || finished.DisplayTruncated
		message.DataError = result.DataError
		if message.DataError == "" {
			message.DataError = finished.DataError
		}
		message.ID = toolResultMessageID(key, result.Text, result.Status)
		p.insertRecoveredToolResult(key, message)
		p.projected[key] = true
		if finished.Image != nil {
			recoveredImages = append(recoveredImages, key)
		}
	}
	for _, key := range recoveredImages {
		p.recoverToolImage(key)
	}
}

func outputReference(finished session.ToolFinished) *messages.OutputReference {
	ref := finished.RawOutput
	if ref == nil {
		ref = finished.Result.Blob
	}
	if ref == nil {
		return nil
	}
	return &messages.OutputReference{Hash: ref.Hash, Size: ref.Size, MediaType: ref.MediaType}
}
