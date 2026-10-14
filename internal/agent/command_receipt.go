package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

var runtimeIDCounter uint64

// planViewVersion is scoped to the immutable plan identity rather than the
// mutable settings revision. A new plan request receives a new ID, so version
// one remains a valid optimistic-concurrency token even when settings change
// while that plan is awaiting a decision.
const planViewVersion uint64 = 1

// Submit admits one typed command through the sole runtime loop. The returned
// receipt distinguishes immediate application from work queued at a safe
// boundary. Transport/context errors are separate from a rejected receipt.
func (a *Agent) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cmd.ID == "" {
		cmd.ID = protocol.CommandID(nextRuntimeID("cmd"))
	}
	if cmd.SessionID == "" {
		cmd.SessionID = protocol.SessionID(a.SessionID())
	}
	normalized, err := cmd.Normalize()
	if err != nil {
		return a.rejectBeforeAdmission(cmd, protocol.ErrorInvalidCommand, err.Error()), nil
	}
	if normalized.SessionID != protocol.SessionID(a.SessionID()) {
		return a.rejectBeforeAdmission(normalized, protocol.ErrorWrongSession, "command targets another session"), nil
	}
	a.ensureRun()
	result := make(chan protocol.Receipt, 1)
	req := runtimeCommand{command: normalized, result: result}
	if isControlCommand(normalized.Type) {
		if err := ctx.Err(); err != nil {
			return protocol.Receipt{}, err
		}
		var signal context.CancelFunc
		req.cancellationTurn, signal = a.captureCommandCancellation(normalized)
		select {
		case <-a.lifecycle.rootCtx.Done():
			return a.rejectBeforeAdmission(normalized, protocol.ErrorClosed, "session is closed"), nil
		case <-ctx.Done():
			return protocol.Receipt{}, ctx.Err()
		case a.owner.control <- controlCommand{request: req}:
		}
		if signal != nil {
			signal()
		}
	} else {
		select {
		case <-a.lifecycle.rootCtx.Done():
			return a.rejectBeforeAdmission(normalized, protocol.ErrorClosed, "session is closed"), nil
		case <-ctx.Done():
			return protocol.Receipt{}, ctx.Err()
		case a.owner.commands <- req:
		default:
			return a.rejectBeforeAdmission(normalized, protocol.ErrorBusy, "command queue is full"), nil
		}
	}

	select {
	case receipt := <-result:
		return receipt, nil
	case <-a.lifecycle.rootCtx.Done():
		// The command was enqueued: closure is not evidence of rejection.
		// The client must replay its original identity to discover admission.
		return protocol.Receipt{}, a.lifecycle.rootCtx.Err()
	case <-ctx.Done():
		return protocol.Receipt{}, ctx.Err()
	}
}

func nextRuntimeID(prefix string) string {
	seq := atomic.AddUint64(&runtimeIDCounter, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), seq)
}

func (a *Agent) rejectedReceipt(cmd protocol.Command, code protocol.ErrorCode, message string) protocol.Receipt {
	r := protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: protocol.ReceiptRejected,
		Revision: a.revision(), Error: &protocol.CommandError{Code: code, Message: message}}
	if cmd.ID == "" {
		return r
	}
	// Rejections participate in command idempotency too.  A caller retrying the
	// same rejected command receives the same receipt, while reusing its ID for
	// a different body remains an explicit conflict in applyCommand.
	digest, digestErr := commandInputDigest(cmd)
	a.mu.Lock()
	if digestErr == nil {
		if prior, exists := a.inbox.seenCommands[cmd.ID]; !exists || prior == digest {
			if _, already := a.inbox.seenReceipts[cmd.ID]; !already {
				a.inbox.seenCommands[cmd.ID] = digest
				a.inbox.seenReceipts[cmd.ID] = r
			}
		}
	}
	a.mu.Unlock()
	return r
}

func (a *Agent) revision() protocol.Revision {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.revisionLocked()
}

func (a *Agent) revisionLocked() protocol.Revision {
	logSeq := a.sequences.logSeq
	// The session store is the durable ordering authority.  Keep the in-memory
	// counter only for compatibility/no-store agents; a persisted session must
	// expose the store cursor so receipts and Watch cursors cannot claim a
	// revision that is not in the log.
	if p := a.services.persistence; p != nil {
		logSeq = uint64(p.CurrentCursor())
	}
	return protocol.Revision{LogSeq: logSeq, SettingsRev: a.sequences.settingsRev,
		ContextRev: a.sequences.contextRev, CatalogVersion: a.sequences.catalogVersion, ViewGeneration: a.sequences.viewGeneration}
}

func (a *Agent) receipt(cmd protocol.Command, status protocol.ReceiptStatus, op protocol.OperationID, err *protocol.CommandError) protocol.Receipt {
	return a.receiptWithFact(cmd, status, op, err, nil)
}

// receiptWithFact is the terminal receipt boundary for asynchronous command
// workers. A caller may supply a pre-built completion fact carrying an output
// blob; ordinary synchronous receipts retain the compact compatibility fact.
func (a *Agent) receiptWithFact(cmd protocol.Command, status protocol.ReceiptStatus, op protocol.OperationID, err *protocol.CommandError, completion *session.CommandCompleted) protocol.Receipt {
	return a.receiptWithFactAndOutput(cmd, status, op, err, completion, "")
}

func (a *Agent) receiptWithFactAndOutput(cmd protocol.Command, status protocol.ReceiptStatus, op protocol.OperationID, err *protocol.CommandError, completion *session.CommandCompleted, output string) protocol.Receipt {
	receipt, _ := a.completeCommand(cmd, status, op, err, completion, output, nil)
	return receipt
}

// completeCommand publishes durable completion and releases its optional owner
// under the watcher/owner locks. The caller must drain a reserved next turn only
// after publishing its remaining operation diagnostics.
func (a *Agent) completeCommand(cmd protocol.Command, status protocol.ReceiptStatus, op protocol.OperationID, err *protocol.CommandError, completion *session.CommandCompleted, output string, owner context.Context) (protocol.Receipt, bool) {
	if cmd.ID != "" && status != protocol.ReceiptScheduled && a.persistenceHandle() != nil {
		fact := session.CommandCompleted{CommandID: string(cmd.ID), Outcome: "rejected"}
		if completion != nil {
			fact = *completion
		} else if status == protocol.ReceiptApplied {
			fact.Outcome = "applied"
		}
		if err != nil {
			fact.Code, fact.Report = string(err.Code), err.Message
		}
		var persistErr error
		if completion != nil {
			persistErr = a.persistCommandCompletedWithOutput(fact, output)
		} else {
			persistErr = a.persistCommandCompletedFact(fact)
		}
		if persistErr != nil {
			status = protocol.ReceiptRejected
			err = &protocol.CommandError{Code: protocol.ErrorInternal, Message: persistErr.Error()}
			op = ""
		}
	}
	a.mu.Lock()
	a.flushStreamsLocked()
	drain := false
	if owner != nil && a.turn.turnCtx == owner {
		a.turn.turnCtx, a.turn.turnCancel = nil, nil
		a.turn.busy, a.lifecycle.settling = false, false
		a.lifecycle.phase = protocol.PhaseIdle
		if a.lifecycle.closing || a.lifecycle.closed {
			a.lifecycle.phase = protocol.PhaseStopping
		}
		drain = owner.Err() == nil && !a.capabilities.settingsPreparing && !a.capabilities.capabilityRevoking && !a.lifecycle.closing && !a.lifecycle.closed && !a.turn.interruptFlag && a.services.persistenceErr == nil && (a.hasTurnStartingInputLocked() || a.goalRunnableLocked())
	}
	a.sequences.logSeq++
	r := protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: status,
		Revision: a.revisionLocked(), OperationID: op, Error: err}
	if cmd.ID != "" {
		a.inbox.seenReceipts[cmd.ID] = r
	}
	view := a.stateLocked()
	state := protocol.Update{Type: protocol.UpdateState, Cursor: protocol.Cursor{LogSeq: r.Revision.LogSeq, ViewGeneration: r.Revision.ViewGeneration}, Revision: r.Revision, State: &view, Catalog: a.catalogUpdateLocked()}
	terminal := protocol.Update{Type: protocol.UpdateReceipt, Cursor: state.Cursor, Revision: r.Revision, Receipt: &r}
	a.enqueueViewsLocked(state, terminal)
	if drain {
		a.turn.busy = true
		a.turn.workStartedAt = time.Now()
		a.turn.workLabel = ""
		a.lifecycle.phase = protocol.PhasePreparing
	}
	a.mu.Unlock()
	return r, drain
}

// restoredCommandReceipt converts a durable command admission into the
// idempotent result that a resumed runtime should return.  An admitted command
// without a terminal fact is deliberately reported as unknown: the process
// may have crossed the external side-effect boundary before crashing, so it
// must never be started again automatically.
func (a *Agent) restoredCommandReceipt(cmd protocol.Command, state durableCommandState) protocol.Receipt {
	if state.digestMismatch {
		return protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID,
			Status: protocol.ReceiptRejected, Revision: a.revision(),
			Error: &protocol.CommandError{Code: protocol.ErrorInvalidCommand,
				Message: "command id was already admitted for a different command body"}}
	}
	status := protocol.ReceiptRejected
	operationID := protocol.OperationID("")
	var commandErr *protocol.CommandError
	if state.scheduled != nil {
		operationID = protocol.OperationID(state.scheduled.OperationID)
		commandErr = &protocol.CommandError{Code: protocol.ErrorInvalidState,
			Message: "command outcome is unknown after restart; it will not be re-run automatically"}
	} else if state.completed != nil {
		operationID = protocol.OperationID(cmd.ID)
		switch state.completed.Outcome {
		case "success", "applied":
			status = protocol.ReceiptApplied
		default:
			code := protocol.ErrorInternal
			if state.completed.Code != "" {
				code = protocol.ErrorCode(state.completed.Code)
			}
			message := state.completed.Report
			if message == "" {
				message = fmt.Sprintf("command completed with outcome %q", state.completed.Outcome)
			}
			commandErr = &protocol.CommandError{Code: code, Message: message}
		}
	}
	r := protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID,
		Status: status, Revision: a.revision(), OperationID: operationID, Error: commandErr}
	a.mu.Lock()
	if digest, err := commandInputDigest(cmd); err == nil {
		a.inbox.seenCommands[cmd.ID] = digest
	}
	a.inbox.seenReceipts[cmd.ID] = r
	a.mu.Unlock()
	return r
}

func durableCommandConflictReceipt(a *Agent, cmd protocol.Command) protocol.Receipt {
	return protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID,
		Status: protocol.ReceiptRejected, Revision: a.revision(),
		Error: &protocol.CommandError{Code: protocol.ErrorInvalidCommand,
			Message: "command id was already admitted for a different command body"}}
}
