package agent

import (
	"fmt"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

func (a *Agent) applyCommand(cmd protocol.Command) protocol.Receipt {
	return a.dispatchCommand(cmd, false)
}

func (a *Agent) dispatchCommand(cmd protocol.Command, owned bool) protocol.Receipt {
	return a.dispatchCommandForTurn(cmd, owned, nil)
}

func (a *Agent) dispatchCommandForTurn(cmd protocol.Command, owned bool, cancellationTurn *uint64) protocol.Receipt {
	normalized, err := cmd.Normalize()
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	digest, err := commandInputDigest(normalized)
	if err != nil {
		return a.rejectedReceipt(normalized, protocol.ErrorInvalidCommand, "encode command identity: "+err.Error())
	}
	a.mu.Lock()
	if old, ok := a.inbox.seenReceipts[normalized.ID]; ok {
		if prior, exists := a.inbox.seenCommands[normalized.ID]; exists && prior != digest {
			a.mu.Unlock()
			return a.rejectedReceipt(normalized, protocol.ErrorInvalidCommand, "command id was already used for a different command")
		}
		a.mu.Unlock()
		return old
	}
	if normalized.SessionID != protocol.SessionID(a.conversation.sessionID) {
		a.mu.Unlock()
		return a.rejectedReceipt(normalized, protocol.ErrorWrongSession, "command targets another session")
	}
	if a.lifecycle.closing || a.lifecycle.closed {
		a.mu.Unlock()
		return a.rejectedReceipt(normalized, protocol.ErrorClosed, "session is closed")
	}
	current := a.revisionLocked()
	if normalized.ExpectedRevision != 0 && normalized.ExpectedRevision != current.LogSeq {
		a.mu.Unlock()
		return a.rejectedReceipt(normalized, protocol.ErrorStaleRevision, fmt.Sprintf("expected revision %d, current %d", normalized.ExpectedRevision, current.LogSeq))
	}
	a.mu.Unlock()
	// A resumed runtime has no in-memory command table.  Consult the typed
	// admission/completion projection before recording this body as a new
	// command.  The full body is compared by digest; it is never recovered from
	// the journal itself.
	state, stateErr := a.durableCommandState(normalized)
	if stateErr != nil {
		a.markPersistenceFailure(stateErr)
		return a.rejectedReceipt(normalized, protocol.ErrorInternal, "read durable command state: "+stateErr.Error())
	}
	if state.digestMismatch {
		return durableCommandConflictReceipt(a, normalized)
	}
	if state.scheduled != nil || state.completed != nil {
		return a.restoredCommandReceipt(normalized, state)
	}
	a.mu.Lock()
	a.inbox.seenCommands[normalized.ID] = digest
	a.mu.Unlock()

	if cancellationTurn != nil && *cancellationTurn != a.currentTurnSeq() {
		return a.receipt(normalized, protocol.ReceiptApplied, "", nil)
	}

	switch normalized.Type {
	case protocol.CommandGoal:
		return a.applyGoalCommand(normalized)
	case protocol.CommandSubmitInput:
		return a.applySubmitInput(normalized)
	case protocol.CommandSetGeneration:
		return a.applyGeneration(normalized, owned)
	case protocol.CommandAnswerQuestion:
		return a.applyAnswerQuestion(normalized)
	case protocol.CommandSetModel:
		return a.applySetModel(normalized, owned)
	case protocol.CommandSetExecutionMode:
		return a.applyExecutionMode(normalized, owned)
	case protocol.CommandSetPermissionPolicy:
		return a.applyPermissionPolicy(normalized, owned)
	case protocol.CommandSetSandboxPolicy:
		return a.applySandboxPolicy(normalized, owned)
	case protocol.CommandRevokeCapabilities:
		return a.applyRevokeCapabilities(normalized, owned)
	case protocol.CommandApproveTool:
		return a.applyApproveTool(normalized)
	case protocol.CommandApprovePlan:
		return a.applyApprovePlan(normalized)
	case protocol.CommandInterrupt:
		a.interrupt()
		return a.receipt(normalized, protocol.ReceiptApplied, "", nil)
	case protocol.CommandStop:
		a.stopTurn()
		return a.receipt(normalized, protocol.ReceiptApplied, "", nil)
	case protocol.CommandClearConversation:
		if a.isBusy() {
			return a.rejectedReceipt(normalized, protocol.ErrorBusy, "cannot clear while a turn is running")
		}
		a.pauseGoal("conversation cleared")
		if err := a.clearHistoryCommand(string(normalized.ID)); err != nil {
			return a.rejectedReceipt(normalized, protocol.ErrorInternal, fmt.Sprintf("persist cleared history: %v", err))
		}
		return a.receipt(normalized, protocol.ReceiptApplied, "", nil)
	case protocol.CommandRemoveMessages:
		if a.isBusy() {
			return a.rejectedReceipt(normalized, protocol.ErrorBusy, "cannot remove messages while a turn is running")
		}
		if err := a.removeMessagesCommand(normalized.Remove.Count, string(normalized.ID)); err != nil {
			return a.rejectedReceipt(normalized, protocol.ErrorInternal, fmt.Sprintf("persist removed messages: %v", err))
		}
		return a.receipt(normalized, protocol.ReceiptApplied, "", nil)
	case protocol.CommandRewindConversation:
		if a.isBusy() {
			return a.rejectedReceipt(normalized, protocol.ErrorBusy, "cannot rewind while a turn is running")
		}
		if err := a.rewindMessagesCommand(normalized.Rewind.Count, string(normalized.ID)); err != nil {
			return a.rejectedReceipt(normalized, protocol.ErrorInternal, fmt.Sprintf("persist rewound history: %v", err))
		}
		return a.receipt(normalized, protocol.ReceiptApplied, "", nil)
	case protocol.CommandCompact:
		if a.isBusy() {
			return a.rejectedReceipt(normalized, protocol.ErrorBusy, "cannot compact while a turn is running")
		}
		return a.scheduleCompact(normalized)
	case protocol.CommandFork:
		if a.isBusy() {
			return a.rejectedReceipt(normalized, protocol.ErrorBusy, "cannot fork while a turn is running")
		}
		return a.scheduleFork(normalized)
	case protocol.CommandExternal:
		return a.applyExternalCommand(normalized)
	case protocol.CommandApply:
		return a.applyApplyCommand(normalized)
	case protocol.CommandCheckpoint:
		return a.applyCheckpointCommand(normalized)
	case protocol.CommandExport:
		return a.applyExportCommand(normalized)
	case protocol.CommandInit:
		return a.applyInitCommand(normalized)
	case protocol.CommandRunWorkflow:
		return a.applyWorkflowCommand(normalized)
	case protocol.CommandQuery:
		return a.applyQueryCommand(normalized)
	case protocol.CommandSetWorkspace:
		return a.applySetWorkspaceCommand(normalized)
	case protocol.CommandReloadSettings:
		return a.applyReloadSettingsCommand(normalized)
	case protocol.CommandTrustProject:
		return a.applyTrustProjectCommand(normalized)
	case protocol.CommandClearMemory:
		return a.applyClearMemoryCommand(normalized)
	default:
		return a.rejectedReceipt(normalized, protocol.ErrorInvalidCommand, "unhandled command")
	}
}

// History commands persist a candidate projection before swapping it into
// the live Agent state, so a
// failed durable commit leaves both history and its token baseline untouched.
func (a *Agent) clearHistoryCommand(turnID string) error {
	a.mu.Lock()
	candidate := make([]messages.Message, 0, len(a.historyLocked()))
	a.mu.Unlock()
	if err := a.persistHistory(candidate, turnID); err != nil {
		return err
	}
	a.mu.Lock()
	a.conversation.recoveryHistory = nil
	a.turnMemory.tokenBaseline.promptTokens = 0
	a.turnMemory.tokenBaseline.historyLen = 0
	a.mu.Unlock()
	a.emit(Event{Type: EventHistoryCleared})
	a.emitStatus("history cleared")
	a.publishState()
	return nil
}

func (a *Agent) removeMessagesCommand(n int, turnID string) error {
	a.mu.Lock()
	candidate := messages.Clone(a.historyLocked())
	if n > 0 && len(a.historyLocked()) > 0 {
		if n > len(candidate) {
			n = len(candidate)
		}
		candidate = sanitizeToolPairs(candidate[:len(candidate)-n])
	}
	a.mu.Unlock()
	if err := a.persistHistory(candidate, turnID); err != nil {
		return err
	}
	a.mu.Lock()
	a.conversation.recoveryHistory = nil
	a.turnMemory.tokenBaseline.promptTokens = 0
	a.turnMemory.tokenBaseline.historyLen = 0
	a.mu.Unlock()
	a.emit(Event{Type: EventHistoryChanged, Text: fmt.Sprintf("removed last %d message(s)", n)})
	a.publishState()
	return nil
}

func (a *Agent) rewindMessagesCommand(n int, turnID string) error {
	a.mu.Lock()
	candidate := messages.Clone(a.historyLocked())
	if n < 0 {
		n = 0
	}
	if n > len(candidate) {
		n = len(candidate)
	}
	candidate = sanitizeToolPairs(candidate[:n])
	a.mu.Unlock()
	if err := a.persistHistory(candidate, turnID); err != nil {
		return err
	}
	a.mu.Lock()
	a.conversation.recoveryHistory = nil
	a.turnMemory.tokenBaseline.promptTokens = 0
	a.turnMemory.tokenBaseline.historyLen = 0
	a.mu.Unlock()
	a.emit(Event{Type: EventHistoryChanged, Text: fmt.Sprintf("rewound to message %d", n)})
	a.publishState()
	return nil
}
