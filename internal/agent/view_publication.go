package agent

import (
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
// TurnDone and the caller must drain it. The event and snapshot share this exact state boundary.
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
	canDrain := !a.capabilities.settingsPreparing && !a.lifecycle.closing && !a.lifecycle.closed && !a.turn.interruptFlag && a.services.persistenceErr == nil && (a.hasTurnStartingInputLocked() || a.goalRunnableLocked())
	a.refreshSnapshotLocked()
	view := a.stateLocked()
	rev := view.Revision
	done := protocol.EventView{
		Kind:      protocol.EventTurnDone,
		SessionID: protocol.SessionID(a.conversation.sessionID),
		TurnID:    turnID,
		StepID:    protocol.StepID(fmt.Sprintf("%d", a.sequences.stepSeq)),
		Phase:     a.lifecycle.phase,

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

func (a *Agent) publishEvent(view protocol.EventView) {
	a.mu.Lock()
	view.SessionID = protocol.SessionID(a.conversation.sessionID)
	if view.TurnID == "" {
		view.TurnID = protocol.TurnID(fmt.Sprintf("%d", a.sequences.turnSeq))
	}
	if view.StepID == "" {
		view.StepID = protocol.StepID(fmt.Sprintf("%d", a.sequences.stepSeq))
	}
	view.Phase = a.lifecycle.phase
	view.PermissionMode = string(a.services.perms.CurrentMode())
	if view.Tool != nil {
		view.CallID = view.Tool.ID
	} else if view.Approval != nil {
		view.CallID = protocol.CallID(view.Approval.ID)
	}
	if view.Kind == protocol.EventError && view.Error == "" {
		view.Error = view.Text
	}
	defer func() {
		a.mu.Unlock()
		a.writeTrace(view)
	}()
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
	if a.publications.streamTimer == nil {
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
	pending, lost := a.publications.streamQueue.Drain(time.Now())
	if len(pending) > 0 {
		a.publishTranscriptPreviewLocked()
	}
	if lost {
		// A bounded preview is still useful in the next snapshot, but its
		// cropped text is no longer a complete delta for existing observers.
		rev := a.revisionLocked()
		a.enqueueViewsLocked(protocol.Update{Type: protocol.UpdateResyncRequired,
			Cursor: protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration}, Revision: rev})
		return
	}
	for _, view := range pending {
		a.publishViewLocked(view)
	}
}
