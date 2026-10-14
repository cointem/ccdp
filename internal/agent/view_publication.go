package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"ccdp/internal/protocol"
)

func (a *Agent) publishState() {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Workers request publication; only the owner builds the business-state
	// view after completing its current durable transition.
	a.publications.statePending = true
	a.enqueueViewsLocked()
}

func (a *Agent) queueStateOwnedLocked() {
	if !a.publications.statePending {
		return
	}
	a.publications.statePending = false
	a.flushStreamsLocked()
	view := a.stateLocked()
	rev := view.Revision
	catalog := a.catalogUpdateLocked()
	update := protocol.Update{Type: protocol.UpdateState, Cursor: protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration}, Revision: rev, State: &view, Catalog: catalog}
	a.enqueueViewsLocked(update)
}

// publishTerminal publishes the final idle snapshot and its TurnDone event as
// one owner-ordered batch, captured under the state lock. No other publication
// can interleave between the terminal snapshot and its event. If a queued input was
// already admitted, a busy reservation for its next turn is published after
// TurnDone and the caller must drain it. No eventView call is made here: it
// would re-enter a.mu and a second, delayed conversion could broadcast an old
// TurnDone after a new user message has started.
func (a *Agent) publishTerminal(turnID protocol.TurnID) bool {
	a.mu.Lock()
	a.flushStreamsLocked()
	// turnFinished established the terminal outcome before entering this
	// helper. Reassert the externally visible idle boundary while the lock is
	// held, then freeze the exact snapshot sent ahead of TurnDone.
	a.turn.busy = false
	if a.lifecycle.closing || a.lifecycle.closed {
		a.lifecycle.phase = protocol.PhaseStopping
	} else {
		a.lifecycle.phase = protocol.PhaseIdle
	}
	a.lifecycle.settling = false
	canDrain := !a.capabilities.settingsPreparing && !a.capabilities.capabilityRevoking && !a.lifecycle.closing && !a.lifecycle.closed && !a.turn.interruptFlag && a.services.persistenceErr == nil && (a.hasTurnStartingInputLocked() || a.goalRunnableLocked())
	a.refreshSnapshotLocked()
	view := a.stateLocked()
	rev := view.Revision
	done := protocol.EventView{
		Kind:           protocol.EventTurnDone,
		SessionID:      protocol.SessionID(a.conversation.sessionID),
		TurnID:         turnID,
		StepID:         protocol.StepID(fmt.Sprintf("%d", a.sequences.stepSeq)),
		Phase:          a.lifecycle.phase,
		Workflow:       a.lifecycle.workflow,
		PermissionMode: string(a.services.perms.CurrentMode()),
	}
	state := protocol.Update{Type: protocol.UpdateState, Cursor: protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration}, Revision: rev, State: &view}
	terminal := protocol.Update{Type: protocol.UpdateStream, Cursor: protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration}, Revision: rev, Event: &done, Text: done.Text}
	a.enqueueViewsLocked(state, terminal)
	if canDrain {
		// Reserve the next turn before releasing the owner lock. A Submit that
		// arrives after TurnDone therefore queues behind this handoff instead of
		// racing the old turn's claim and starting a second turn.
		a.turn.busy = true
		a.turn.workStartedAt = time.Now()
		a.turn.workLabel = ""
		a.lifecycle.phase = protocol.PhasePreparing
		next := a.stateLocked()
		nextRev := next.Revision
		nextState := protocol.Update{Type: protocol.UpdateState, Cursor: protocol.Cursor{LogSeq: nextRev.LogSeq, ViewGeneration: nextRev.ViewGeneration}, Revision: nextRev, State: &next}
		a.enqueueViewsLocked(nextState)
	}
	a.mu.Unlock()
	return canDrain
}

func (a *Agent) publishReceipt(receipt protocol.Receipt) {
	a.mu.Lock()
	defer a.mu.Unlock()
	update := protocol.Update{Type: protocol.UpdateReceipt, Cursor: protocol.Cursor{LogSeq: receipt.Revision.LogSeq, ViewGeneration: receipt.Revision.ViewGeneration}, Revision: receipt.Revision, Receipt: &receipt}
	a.enqueueViewsLocked(update)
}

func (a *Agent) publishEvent(ev Event) {
	view := a.eventView(ev)
	a.mu.Lock()
	defer a.mu.Unlock()
	streaming := (view.Kind == protocol.EventStream && view.Text != "") || view.Kind == protocol.EventReasoning || view.Kind == protocol.EventToolProgress
	if !streaming {
		a.flushStreamsLocked()
	}
	a.transcript.Event(view)
	if !streaming {
		a.publishViewLocked(view)
		return
	}
	delay := a.publications.streamQueue.Add(view, time.Now())
	if delay <= 0 {
		a.flushStreamsLocked()
		return
	}
	if a.publications.streamTimer == nil && a.owner != nil {
		a.publications.streamTimerGeneration++
		generation := a.publications.streamTimerGeneration
		a.publications.streamTimer = time.AfterFunc(delay, func() {
			select {
			case a.owner.control <- streamsReady{generation: generation}:
			case <-a.owner.done:
			}
		})
	}
}

// Caller holds mu. All progress sources use the same ordered publisher.
func (a *Agent) publishViewLocked(view protocol.EventView) {
	rev := a.revisionLocked()
	if view.Transcript == nil {
		view.Transcript = a.transcript.EventItem(view)
	}
	if view.Transcript == nil && (view.Kind == protocol.EventReasoning || view.Kind == protocol.EventStream && view.Text != "" || view.Kind == protocol.EventToolProgress) {
		// A durable commit already replaced this live item. The final snapshot
		// is authoritative; do not replay an orphan delta after its replacement.
		return
	}
	update := protocol.Update{Type: protocol.UpdateStream, Cursor: protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration}, Revision: rev, Event: &view, Text: view.Text}
	a.enqueueViewsLocked(update)
}

func (a *Agent) flushStreamsLocked() {
	if a.publications.streamTimer != nil {
		a.publications.streamTimer.Stop()
		a.publications.streamTimer = nil
	}
	pending := a.publications.streamQueue.Drain(time.Now())
	if len(pending) > 0 {
		a.publishTranscriptPreviewLocked()
	}
	for _, view := range pending {
		a.publishViewLocked(view)
	}
}

// eventView projects an internal runtime event into the typed Watch protocol.
func (a *Agent) eventView(ev Event) protocol.EventView {
	a.mu.Lock()
	turnID := protocol.TurnID(ev.TurnID)
	if turnID == "" {
		turnID = protocol.TurnID(fmt.Sprintf("%d", a.sequences.turnSeq))
	}
	view := protocol.EventView{SessionID: protocol.SessionID(a.conversation.sessionID), TurnID: turnID, StepID: protocol.StepID(fmt.Sprintf("%d", a.sequences.stepSeq)), Phase: a.lifecycle.phase, Workflow: a.lifecycle.workflow, MessageID: ev.MessageID, Text: ev.Text}
	mode := a.services.perms.CurrentMode()
	a.mu.Unlock()
	view.PermissionMode = string(mode)
	switch ev.Type {
	case EventGoalChanged:
		view.Kind = protocol.EventGoalChanged
		view.Goal = protocol.CloneGoal(ev.Goal)
	case EventStatus:
		view.Kind = protocol.EventStatus
	case EventUserMsg:
		view.Kind = protocol.EventUserMessage
	case EventStream:
		view.Kind = protocol.EventStream
	case EventReasoning:
		view.Kind = protocol.EventReasoning
	case EventToolStart:
		view.Kind = protocol.EventToolStarted
	case EventToolStream:
		view.Kind = protocol.EventToolProgress
	case EventToolResult:
		view.Kind = protocol.EventToolResult
	case EventApproval:
		view.Kind = protocol.EventApprovalRequest
	case EventQuestion:
		view.Kind = protocol.EventQuestionRequest
		view.Question = protocol.CloneQuestionRequest(ev.Question)
	case EventPlan:
		view.Kind = protocol.EventPlanReady
	case EventError:
		view.Kind, view.Error = protocol.EventError, ev.Text
	case EventTurnDone:
		view.Kind = protocol.EventTurnDone
	case EventUsage:
		view.Kind = protocol.EventUsage
	case EventPlanModeChanged:
		view.Kind = protocol.EventStateChanged
		if ev.PlanMode {
			view.ExecutionMode = protocol.ExecutionModePlan
		} else {
			view.ExecutionMode = protocol.ExecutionModeExecute
		}
	case EventModeChanged, EventSandboxChanged, EventHistoryChanged, EventHistoryCleared, EventCompacted, EventSessionChanged:
		view.Kind = protocol.EventStateChanged
	default:
		view.Kind = protocol.EventStateChanged
	}
	if ev.Tool != nil {
		args, _ := json.Marshal(ev.Tool.Args)
		view.Tool = &protocol.ToolView{ID: protocol.CallID(ev.Tool.ID), Name: ev.Tool.Name, Status: ev.Tool.Status, Args: args, Output: ev.Tool.Output}
		view.CallID = protocol.CallID(ev.Tool.ID)
	}
	if ev.Approval != nil {
		view.Approval = &protocol.ApprovalView{ID: ev.Approval.ID, Tool: ev.Approval.Tool,
			Reason: ev.Approval.Reason, Args: approvalArgsView(ev.Approval), Capabilities: append([]protocol.CapabilityRequest(nil), ev.Approval.Capabilities...)}
		view.CallID = protocol.CallID(ev.Approval.ID)
	}
	if ev.Plan != nil {
		view.Plan = &protocol.PlanView{ID: ev.Plan.ID, Text: ev.Plan.Plan, Version: planViewVersion}
	}
	if ev.Usage != nil {
		view.Usage = &protocol.UsageSnapshot{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens, CachedTokens: ev.Usage.CachedTokens, Cache: ev.Usage.Cache, TurnCount: ev.Usage.TurnCount}
	}
	return view
}
