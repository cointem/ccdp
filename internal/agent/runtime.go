package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/events"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

var runtimeIDCounter uint64

// planViewVersion is scoped to the immutable plan identity rather than the
// mutable settings revision. A new plan request receives a new ID, so version
// one remains a valid optimistic-concurrency token even when settings change
// while that plan is awaiting a decision.
const planViewVersion uint64 = 1

func (a *Agent) eventLoop() {
	defer a.eventWG.Done()
	for {
		select {
		case <-a.rootCtx.Done():
			return
		case <-a.eventDone:
			return
		case ev := <-a.eventQueue:
			if a.events == nil {
				continue
			}
			select {
			case a.events <- ev:
			case <-a.rootCtx.Done():
				return
			case <-a.eventDone:
				return
			}
		}
	}
}

func (a *Agent) ensureRun() {
	a.mu.Lock()
	started := a.runStartedFlag
	closed := a.closed || a.closing
	a.mu.Unlock()
	if !started && !closed {
		go a.Run()
	}
}

// Context is the root cancellation context owned by the runtime. It is
// read-only to consumers; cancellation is performed by Interrupt/Close.
func (a *Agent) Context() context.Context { return a.rootCtx }

// Done closes when Close has completed all runtime-owned work and resources.
func (a *Agent) Done() <-chan struct{} { return a.closeDone }

// Close is idempotent and joins the runtime before closing providers and
// session resources. The external Event channel is deliberately not closed:
// its caller owns that channel and may use it for more than one adapter.
func (a *Agent) Close() { _ = a.CloseContext(context.Background()) }

// CloseContext cancels all turn/approval work, waits for it to stop, and then
// releases session resources. A context deadline reports a cleanup error while
// preserving the idempotent close state; a second CloseContext observes the
// same result.
func (a *Agent) CloseContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.closeOnce.Do(func() { go a.closeAsync() })
	select {
	case <-a.closeDone:
		return a.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) closeAsync() {
	a.mu.Lock()
	a.closing = true
	a.phase = protocol.PhaseStopping
	a.stop = true
	cancel := a.turnCancel
	compactCancel := a.compactCancel
	rootCancel := a.rootCancel
	runStarted := a.runStartedFlag
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if compactCancel != nil {
		compactCancel()
	}
	if rootCancel != nil {
		rootCancel()
	}
	if a.supervisorOwner && a.supervisor != nil {
		a.supervisor.close()
	}

	a.turnWG.Wait()
	a.operationWG.Wait()
	if !runStarted {
		// Starting a no-op loop during shutdown closes the race where Close wins
		// just before a concurrently requested Run begins. We then join the same
		// runDone channel in both cases, so no owner goroutine can outlive Close.
		go a.Run()
	}
	<-a.runDone
	a.finishClose()
}

func (a *Agent) finishClose() {
	var closeErr error
	// rootCtx is canceled before this method runs, so lifecycle hooks must use
	// an independent, bounded cleanup context.  A hung hook must not turn Close
	// into an unbounded wait or make the later owner releases unreachable.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	// Stop event delivery before releasing trace/provider resources. Producers
	// are already joined, so no event can be enqueued after this point.
	select {
	case <-a.eventDone:
	default:
		close(a.eventDone)
	}
	a.eventWG.Wait()

	a.sessionReg.RunDispose(a.sessionID)
	a.evbus.Emit(events.TopicSessionDisposed, events.SessionEvent{ID: a.sessionID})
	if hookOut := a.runHookWithJournal(cleanupCtx, hooks.EventSessionEnd, func(hookCtx context.Context) hooks.Output {
		return a.hooks.SessionEnd(hookCtx)
	}); hookOut.Decision == hooks.DecisionDeny || hookOut.Decision == hooks.DecisionBlock {
		if hookOut.Reason != "" {
			closeErr = errors.Join(closeErr, fmt.Errorf("session end hook: %s", hookOut.Reason))
		}
	}
	if hookOut := a.runHookWithJournal(cleanupCtx, hooks.EventStop, func(hookCtx context.Context) hooks.Output {
		return a.hooks.Stop(hookCtx)
	}); hookOut.Decision == hooks.DecisionDeny || hookOut.Decision == hooks.DecisionBlock {
		if hookOut.Reason != "" {
			closeErr = errors.Join(closeErr, fmt.Errorf("stop hook: %s", hookOut.Reason))
		}
	}
	if a.mcp != nil {
		if err := a.mcp.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("MCP close: %w", err))
		}
	}
	if a.resources != nil {
		if err := a.resources.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("resources close: %w", err))
		}
	}
	if err := a.persistSessionClosed("success"); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("session close fact: %w", err))
	}
	if a.finalizeRun != nil {
		closeErr = errors.Join(closeErr, a.finalizeRun(closeErr))
	}
	if err := a.closePersistence(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("persistence close: %w", err))
	}
	a.closeTrace()
	if a.host != nil {
		if err := a.host.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("extension host close: %w", err))
		}
	}
	if err := cleanupCtx.Err(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("cleanup deadline: %w", err))
	}

	a.mu.Lock()
	a.closeErr = closeErr
	a.closed = true
	a.phase = protocol.PhaseClosed
	a.mu.Unlock()
	a.publishState()
	a.closeWatchers()
	close(a.closeDone)
}

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
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error()), nil
	}
	if normalized.SessionID != protocol.SessionID(a.SessionID()) {
		return a.rejectedReceipt(normalized, protocol.ErrorWrongSession, "command targets another session"), nil
	}
	a.ensureRun()
	result := make(chan protocol.Receipt, 1)
	req := runtimeCommand{command: normalized, result: result}
	select {
	case <-a.rootCtx.Done():
		return a.rejectedReceipt(normalized, protocol.ErrorClosed, "session is closed"), nil
	case <-ctx.Done():
		return protocol.Receipt{}, ctx.Err()
	case a.commands <- req:
	default:
		return a.rejectedReceipt(normalized, protocol.ErrorBusy, "command queue is full"), nil
	}
	select {
	case receipt := <-result:
		return receipt, nil
	case <-a.rootCtx.Done():
		return a.rejectedReceipt(normalized, protocol.ErrorClosed, "session is closed"), nil
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
		if prior, exists := a.seenCommands[cmd.ID]; !exists || prior == digest {
			if _, already := a.seenReceipts[cmd.ID]; !already {
				a.seenCommands[cmd.ID] = digest
				a.seenReceipts[cmd.ID] = r
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
	logSeq := a.logSeq
	// The session store is the durable ordering authority.  Keep the in-memory
	// counter only for compatibility/no-store agents; a persisted session must
	// expose the store cursor so receipts and Watch cursors cannot claim a
	// revision that is not in the log.
	if p := a.persistence; p != nil {
		logSeq = uint64(p.CurrentCursor())
	}
	return protocol.Revision{LogSeq: logSeq, SettingsRev: a.settingsRev,
		ContextRev: a.contextRev, CatalogVersion: a.catalogVersion, ViewGeneration: a.viewGeneration}
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
	a.logSeq++
	r := protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: status,
		Revision: a.revisionLocked(), OperationID: op, Error: err}
	if cmd.ID != "" {
		a.seenReceipts[cmd.ID] = r
	}
	a.mu.Unlock()
	// Completion advances the revision after command handlers publish their
	// settings/history changes. Publish that final boundary too, otherwise
	// Watch clients keep a snapshot one revision behind and their next guarded
	// command is rejected even when nothing else has changed.
	a.publishState()
	a.publishReceipt(r)
	return r
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
		a.seenCommands[cmd.ID] = digest
	}
	a.seenReceipts[cmd.ID] = r
	a.mu.Unlock()
	return r
}

func durableCommandConflictReceipt(a *Agent, cmd protocol.Command) protocol.Receipt {
	return protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID,
		Status: protocol.ReceiptRejected, Revision: a.revision(),
		Error: &protocol.CommandError{Code: protocol.ErrorInvalidCommand,
			Message: "command id was already admitted for a different command body"}}
}

func (a *Agent) applyCommand(cmd protocol.Command) protocol.Receipt {
	normalized, err := cmd.Normalize()
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	digest, err := commandInputDigest(normalized)
	if err != nil {
		return a.rejectedReceipt(normalized, protocol.ErrorInvalidCommand, "encode command identity: "+err.Error())
	}
	a.mu.Lock()
	if old, ok := a.seenReceipts[normalized.ID]; ok {
		if prior, exists := a.seenCommands[normalized.ID]; exists && prior != digest {
			a.mu.Unlock()
			return a.rejectedReceipt(normalized, protocol.ErrorInvalidCommand, "command id was already used for a different command")
		}
		a.mu.Unlock()
		return old
	}
	if normalized.SessionID != protocol.SessionID(a.sessionID) {
		a.mu.Unlock()
		return a.rejectedReceipt(normalized, protocol.ErrorWrongSession, "command targets another session")
	}
	if a.closing || a.closed {
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
	a.seenCommands[normalized.ID] = digest
	a.mu.Unlock()

	switch normalized.Type {
	case protocol.CommandSubmitInput:
		return a.applySubmitInput(normalized)
	case protocol.CommandSetGeneration:
		return a.applyGeneration(normalized)
	case protocol.CommandAnswerQuestion:
		return a.applyAnswerQuestion(normalized)
	case protocol.CommandSetModel:
		return a.applySetModel(normalized)
	case protocol.CommandSetExecutionMode:
		return a.applyExecutionMode(normalized)
	case protocol.CommandSetPermissionPolicy:
		return a.applyPermissionPolicy(normalized)
	case protocol.CommandSetSandboxPolicy:
		return a.applySandboxPolicy(normalized)
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
	case protocol.CommandSaveSession:
		return a.applySaveSessionCommand(normalized)
	default:
		return a.rejectedReceipt(normalized, protocol.ErrorInvalidCommand, "unhandled command")
	}
}

// The legacy history helpers predate the typed command loop and deliberately
// swallow Save errors for compatibility callers. Canonical commands persist a
// candidate projection before swapping it into the live Agent state, so a
// failed durable commit leaves both history and its token baseline untouched.
func (a *Agent) clearHistoryCommand(turnID string) error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.mu.Lock()
	candidate := make([]messages.Message, 0, len(a.history))
	a.mu.Unlock()
	if err := a.persistHistory(candidate, turnID); err != nil {
		return err
	}
	a.mu.Lock()
	a.history = candidate
	a.tokenBaseline.promptTokens = 0
	a.tokenBaseline.historyLen = 0
	a.mu.Unlock()
	a.emit(Event{Type: EventHistoryCleared})
	a.emitStatus("history cleared")
	a.publishState()
	return nil
}

func (a *Agent) removeMessagesCommand(n int, turnID string) error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.mu.Lock()
	candidate := cloneMessages(a.history)
	if n > 0 && len(a.history) > 0 {
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
	a.history = candidate
	a.tokenBaseline.promptTokens = 0
	a.tokenBaseline.historyLen = 0
	a.mu.Unlock()
	a.emit(Event{Type: EventHistoryChanged, Text: fmt.Sprintf("removed last %d message(s)", n)})
	a.publishState()
	return nil
}

func (a *Agent) rewindMessagesCommand(n int, turnID string) error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.mu.Lock()
	candidate := cloneMessages(a.history)
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
	a.history = candidate
	a.tokenBaseline.promptTokens = 0
	a.tokenBaseline.historyLen = 0
	a.mu.Unlock()
	a.emit(Event{Type: EventHistoryChanged, Text: fmt.Sprintf("rewound to message %d", n)})
	a.publishState()
	return nil
}

func (a *Agent) scheduleCompact(cmd protocol.Command) protocol.Receipt {
	ctx, cancel := context.WithCancel(a.rootCtx)
	a.mu.Lock()
	if a.closing || a.closed {
		a.mu.Unlock()
		cancel()
		return a.rejectedReceipt(cmd, protocol.ErrorClosed, "session is closed")
	}
	a.busy = true
	a.phase = protocol.PhaseCompacting
	// Compaction is a new operation boundary. Clear cancellation inherited from
	// an earlier completed turn here; an interrupt delivered after this lock is
	// observed by the worker and any turn it may hand off.
	a.interruptFlag = false
	a.stop = false
	a.compactCancel = cancel
	a.operationWG.Add(1)
	a.mu.Unlock()
	a.publishState()
	receipt := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
	go func() {
		defer a.operationWG.Done()
		defer cancel()
		a.compactContext(ctx)
		canceled := ctx.Err() != nil
		a.mu.Lock()
		a.compactCancel = nil
		closing := a.closing || a.closed
		canDrain := !closing && !canceled && a.persistenceErr == nil
		nextTurnID := fmt.Sprintf("turn-%d", a.turnSeq+1)
		a.mu.Unlock()
		if !canDrain {
			a.mu.Lock()
			a.busy = false
			if a.closing || a.closed {
				a.phase = protocol.PhaseStopping
			} else {
				a.phase = protocol.PhaseIdle
			}
			a.mu.Unlock()
			a.publishState()
			return
		}

		// A typed input is not removed from the in-memory inbox until its
		// durable delivery fact commits. This keeps a failed commit visible and
		// prevents a compaction worker from starting a turn that cannot be
		// recovered after restart.
		next, ok, deliveryErr := a.claimPendingInputAndAppend(nextTurnID, "")
		if deliveryErr != nil {
			a.markPersistenceFailure(deliveryErr)
			a.mu.Lock()
			a.busy = false
			if !a.closing && !a.closed {
				a.phase = protocol.PhaseIdle
			}
			a.mu.Unlock()
			a.emit(Event{Type: EventError, Text: "queued input delivery failed: " + deliveryErr.Error()})
			a.publishState()
			return
		}
		if !ok || ctx.Err() != nil {
			a.mu.Lock()
			a.busy = false
			if a.closing || a.closed {
				a.phase = protocol.PhaseStopping
			} else {
				a.phase = protocol.PhaseIdle
			}
			a.mu.Unlock()
			a.publishState()
			return
		}
		a.mu.Lock()
		if a.closing || a.closed || a.stop || a.interruptFlag || a.persistenceErr != nil {
			a.busy = false
			if !a.closing && !a.closed {
				a.phase = protocol.PhaseIdle
			}
			a.mu.Unlock()
			a.publishState()
			return
		}
		a.turnSeq++
		a.turnWG.Add(1)
		// The queued input becomes the next turn only under this owner lock.
		// Reset the old terminal flags before publishing the new preparing
		// state; a later Interrupt/Stop cannot be erased by runTurn because it
		// no longer clears them asynchronously.
		a.interruptFlag = false
		a.stop = false
		a.phase = protocol.PhasePreparing
		a.mu.Unlock()
		a.publishState()
		go a.runTurn(next)
	}()
	return receipt
}

func (a *Agent) scheduleFork(cmd protocol.Command) protocol.Receipt {
	a.mu.Lock()
	if a.closing || a.closed {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorClosed, "session is closed")
	}
	a.operationWG.Add(1)
	a.mu.Unlock()
	receipt := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
	go func() {
		defer a.operationWG.Done()
		id, err := a.Fork(cmd.Fork.Count)
		if err != nil {
			// Fork is an independent operation and may finish after a new turn
			// has started. Keep its diagnostic out of the turn outcome bridge.
			a.emitStatus("fork failed: %v", err)
			return
		}
		a.emitStatus("forked session %s", id)
		a.publishState()
	}()
	return receipt
}

func (a *Agent) isBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy || a.settling
}

func (a *Agent) setPhase(phase protocol.RuntimePhase) {
	a.mu.Lock()
	if !a.closed {
		a.phase = phase
	}
	a.mu.Unlock()
	a.publishState()
}

func (a *Agent) bindingEndpointLocked(model string) string {
	baseURL, _ := a.cfg.EndpointFor(model)
	return baseURL
}

func (a *Agent) stopTurn() {
	a.mu.Lock()
	a.stop = true
	a.phase = protocol.PhaseStopping
	cancel := a.turnCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.emitStatus("stop requested")
	a.publishState()
}

func (a *Agent) applySubmitInput(cmd protocol.Command) protocol.Receipt {
	input := *cmd.Input
	if err := protocol.ValidateInputImages(input.Images); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	// applySubmitInput is also used by the bounded /apply worker after it has
	// read a plan file, so it can be reached without a second Command.Normalize
	// call. Keep the protocol admission limit at this inner boundary as well;
	// otherwise that compatibility path could bypass the user-input bound.
	if len(input.Text) > protocol.MaxSubmitInputTextBytes {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand,
			fmt.Sprintf("input text exceeds %d bytes", protocol.MaxSubmitInputTextBytes))
	}
	inputID := input.ID
	if inputID == "" {
		inputID = protocol.InputID(cmd.ID)
	}
	input.ID = inputID
	if input.Strategy == "" {
		input.Strategy = protocol.InputSteer
	}
	inputDigest, digestErr := submitInputDigest(input)
	if digestErr != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "encode input identity: "+digestErr.Error())
	}
	createdAt := time.Now().UTC()

	// Queue admission, durable delivery, and the corresponding history append
	// share one Agent projection barrier. Save/turn settlement cannot observe a
	// half-admitted input or overwrite the just-committed history cache.
	a.persistMu.Lock()
	a.mu.Lock()
	if old, ok := a.seenInputs[inputID]; ok {
		prior := a.seenInputBody[inputID]
		if prior != inputDigest {
			a.mu.Unlock()
			a.persistMu.Unlock()
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "input id was already used for different text or strategy")
		}
		a.mu.Unlock()
		a.persistMu.Unlock()
		return old
	}
	turnSeq := a.turnSeq
	closing := a.closing || a.closed
	if !a.busy && !a.settling {
		// Initialize the new turn at admission, before any durable I/O. A
		// cancellation delivered after this lock must remain set through the
		// eventual start; runTurn deliberately does not reset it.
		a.interruptFlag = false
		a.stop = false
	}
	a.mu.Unlock()
	if closing {
		a.persistMu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorClosed, "session is closed")
	}
	if err := a.persistenceFailure(); err != nil {
		a.persistMu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, fmt.Sprintf("session persistence unavailable: %v", err))
	}
	if turnSeq == 0 {
		turnSeq = 1
	}
	// Capture all referenced images at the durable-input boundary. The path is
	// only an input locator; subsequent requests use the frozen bytes/blob.
	attachments, captureErr := a.freezeImageAttachments(input.Text)
	if captureErr != nil {
		a.persistMu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, captureErr.Error())
	}
	// Clipboard locators are generated only after all user-supplied Markdown
	// paths have passed workspace checks. They are never opened as files.
	var imageBytes int
	for _, attachment := range attachments {
		imageBytes += len(attachment.Data)
	}
	for i, img := range input.Images {
		imageBytes += len(img.Data)
		if imageBytes > maxAttachmentBytes {
			a.persistMu.Unlock()
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "images exceed total attachment limit")
		}
		path := fmt.Sprintf("ccdp-clipboard:%d", i+1)
		if input.Text != "" {
			input.Text += "\n"
		}
		input.Text += fmt.Sprintf("![Image %d](%s)", i+1, path)
		attachments = append(attachments, messages.ImageAttachment{Path: path, MediaType: img.MediaType, Data: append([]byte(nil), img.Data...)})
	}
	if err := a.persistInputWithDigest(string(inputID), input.Text, string(input.Strategy), fmt.Sprintf("turn-%d", turnSeq), createdAt, inputDigest, attachments...); err != nil {
		a.persistMu.Unlock()
		if errors.Is(err, session.ErrTooLarge) {
			// Input text is user-controlled data. A JSONL transaction which is
			// too large is a recoverable admission rejection (including escaped
			// control/HTML bytes that expand during JSON encoding), not evidence
			// that the session log is broken. Do not latch the runtime.
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "input exceeds the session transaction size limit")
		}
		a.markPersistenceFailure(err)
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, fmt.Sprintf("persist input: %v", err))
	}

	a.mu.Lock()
	// A turn may settle while the durable admission is in flight. Re-read the
	// owner state after the commit so the input is either queued for that turn
	// or starts the next one, but never starts before it is durable.
	if a.closing || a.closed {
		a.mu.Unlock()
		a.persistMu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorClosed, "session is closed")
	}
	if a.inputAttachments == nil {
		a.inputAttachments = make(map[protocol.InputID]frozenInputAttachments)
	}
	a.inputAttachments[inputID] = frozenInputAttachments{Attachments: cloneImageAttachments(attachments), Frozen: true}
	if a.busy || a.settling {
		a.pendingInputs = append(a.pendingInputs, protocol.InputView{ID: inputID, Text: input.Text, Strategy: input.Strategy, State: "queued", CreatedAt: createdAt})
		a.syncLegacyPendingLocked()
		a.mu.Unlock()
		a.persistMu.Unlock()
		a.emitStatus("queued (will be delivered at a safe turn boundary)")
		r := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
		a.mu.Lock()
		a.seenInputs[inputID] = r
		a.seenInputBody[inputID] = inputDigest
		a.mu.Unlock()
		a.publishState()
		return r
	}
	nextTurnID := fmt.Sprintf("turn-%d", a.turnSeq+1)
	a.mu.Unlock()
	if err := a.persistInputDelivered(string(inputID), nextTurnID); err != nil {
		a.markPersistenceFailure(err)
		a.persistMu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, fmt.Sprintf("deliver input: %v", err))
	}
	turnInput := newTurnInput(inputID, input.Text, input.Strategy, createdAt)
	turnInput.ImageAttachments = cloneImageAttachments(attachments)
	turnInput.ImagesFrozen = true
	turnInput.historyAppended = true
	deliveredMessage := turnInput.message()
	if err := a.appendHistoryLocked(deliveredMessage); err != nil {
		a.persistMu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, fmt.Sprintf("persist input history: %v", err))
	}
	a.persistMu.Unlock()
	a.evbus.Emit(events.TopicMessageAdded, events.MessageEvent{Role: string(deliveredMessage.Role), Content: deliveredMessage.Content})
	a.startTurnInput(turnInput)
	r := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
	a.mu.Lock()
	a.seenInputs[inputID] = r
	a.seenInputBody[inputID] = inputDigest
	a.mu.Unlock()
	return r
}

func (a *Agent) makeBinding(model string) (modelBinding, error) {
	a.mu.Lock()
	cfg := cloneConfig(a.cfg)
	version := a.activeBinding.version + 1
	models := a.models
	a.mu.Unlock()
	return providerBinding(cfg, models, strings.TrimSpace(model), version)
}

func (a *Agent) applySetModel(cmd protocol.Command) protocol.Receipt {
	// Model candidates and the step-boundary application form one state
	// machine. Serialize the capture/commit/publish sequence so a step cannot
	// commit B after an idle SetModel has already superseded it (or vice versa).
	// This lock is intentionally narrower than a.mu and may cover durable I/O;
	// ordinary Agent state readers use its read side for a coherent snapshot.
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()
	binding, err := a.makeBinding(strings.TrimSpace(cmd.Model.Model))
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	a.mu.Lock()
	cfg := cloneConfig(a.cfg)
	sourceRevision := a.settingsRev
	supersededChangeID := ""
	if a.pendingBinding != nil {
		// A pending binding is a candidate regardless of whether an older
		// snapshot populated its revision.  Treating only higher revisions as
		// pending leaves a legacy/stale candidate alive when an idle command
		// replaces it, which can then be re-applied at the next step boundary.
		if a.pendingSettingsRevision > sourceRevision {
			sourceRevision = a.pendingSettingsRevision
		}
		supersededChangeID = a.pendingSettingsChangeID
	}
	nextRevision := sourceRevision + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	settings := a.sessionSettingsLocked()
	busy := a.busy
	settings.Model = binding.model
	settings.Provider = binding.provider
	settings.Endpoint, _ = sanitizeRequestEndpoint(binding.endpoint)
	a.mu.Unlock()
	scheduledAdmission := false
	if busy {
		if err := a.persistSettingsScheduledFactWithCancel(string(cmd.ID), settings, nextRevision, supersededChangeID); err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
		}
		scheduledAdmission = true
		if supersededChangeID != "" && supersededChangeID != string(cmd.ID) {
			a.finishSupersededSettingsReceipt(supersededChangeID)
		}
		// The turn may have reached its terminal boundary while the durable
		// admission was in flight. Only publish a pending binding when the
		// runtime is still busy; otherwise continue through the idle Applied
		// path below and commit SettingsChanged before mutating live state.
		a.mu.Lock()
		stillBusy := a.busy
		if stillBusy {
			a.pendingBinding = &binding
			a.pendingSettingsRevision = nextRevision
			a.pendingSettingsChangeID = string(cmd.ID)
		}
		a.mu.Unlock()
		if stillBusy {
			a.emitStatus("model %s staged for the next step", binding.model)
			a.publishState()
			return a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
		}
	}
	// If the turn became idle after we durably admitted the candidate, close
	// that same admission in the SettingsChanged + CommandCompleted transaction
	// rather than writing a bare SettingsChanged fact. Otherwise replay would
	// retain an already-applied model command as an unresolved Scheduled one.
	var applyErr error
	if scheduledAdmission {
		applyErr = a.persistScheduledSettingsAppliedFact(string(cmd.ID), settings, nextRevision)
	} else if supersededChangeID != "" {
		// An idle replacement must close the old scheduled candidate and publish
		// the new active settings atomically.  In particular, do not leave the
		// old candidate to be reconstructed by a later step or resume.
		applyErr = a.persistSettingsAppliedFact(string(cmd.ID), settings, nextRevision, supersededChangeID)
	} else {
		applyErr = a.persistSettingsFact(settings, nextRevision)
	}
	if applyErr != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, applyErr.Error())
	}
	a.mu.Lock()
	a.applyBindingLocked(binding)
	// applyBindingLocked intentionally preserves a different pending binding
	// for the normal step-boundary path.  This command has just superseded that
	// binding at an idle boundary, so clear it explicitly after the durable
	// replacement transaction succeeds.
	a.pendingBinding = nil
	a.settingsRev = nextRevision
	a.pendingSettingsRevision = 0
	a.pendingSettingsChangeID = ""
	a.mu.Unlock()
	a.models.Register(binding.client)
	if binding.routeKind == "http" {
		_, apiKey := cfg.EndpointFor(binding.model)
		a.models.RouteDefault(binding.model, binding.provider, binding.endpoint, apiKey)
	} else {
		a.models.Route(binding.model, binding.provider)
	}
	a.emit(Event{Type: EventModeChanged, Text: "model:" + binding.model})
	a.emitStatus("model switched to %s", binding.model)
	a.publishState()
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
}

// finishSupersededSettingsReceipt closes the in-memory admission receipt
// after the cancellation and replacement facts have committed.  It does not
// write another CommandCompleted event: the durable transaction in
// persistSettingsScheduledFactWithCancel is already the single terminal fact
// for the old command.
func (a *Agent) finishSupersededSettingsReceipt(commandID string) {
	if strings.TrimSpace(commandID) == "" {
		return
	}
	a.mu.Lock()
	r := protocol.Receipt{CommandID: protocol.CommandID(commandID), SessionID: protocol.SessionID(a.sessionID),
		Status: protocol.ReceiptRejected, Revision: a.revisionLocked(), OperationID: protocol.OperationID(commandID),
		Error: &protocol.CommandError{Code: protocol.ErrorInvalidState, Message: "settings change was superseded before its step boundary"}}
	a.seenReceipts[protocol.CommandID(commandID)] = r
	a.mu.Unlock()
	a.publishReceipt(r)
}

func (a *Agent) applyBindingLocked(binding modelBinding) {
	a.activeBinding = binding
	a.client = binding.client
	a.primaryClient = binding.client
	a.primaryEndpoint = binding.endpoint
	a.primaryRouteKind = binding.routeKind
	a.activeModel = binding.model
	a.cfg.Model = binding.model
	a.baseCfg.Model = binding.model
	a.modelSwitchMsg = "# Model\nYou have been switched to the model \"" + binding.model + "\". Adjust your responses to its capabilities."
	if a.pendingBinding != nil && a.pendingBinding.model == binding.model {
		a.pendingBinding = nil
	}
}

// restorePendingSettings rehydrates an admitted-but-not-yet-applied model
// change from replay. It never changes the active binding; the normal checked
// step boundary will commit SettingsChanged and apply the candidate before the
// next provider request.
func (a *Agent) restorePendingSettings(candidate session.SettingsScheduled) error {
	model := strings.TrimSpace(candidate.Settings.Model)
	if model == "" {
		return nil
	}
	binding, err := a.makeBinding(model)
	if err != nil {
		return err
	}
	revision := candidate.SourceRevision + 1
	if revision == 0 {
		revision = 1
	}
	a.mu.Lock()
	if binding.model == a.activeBinding.model && candidate.Settings.Provider == a.activeBinding.provider {
		a.mu.Unlock()
		return nil
	}
	a.pendingBinding = &binding
	a.pendingSettingsRevision = revision
	a.pendingSettingsChangeID = candidate.ChangeID
	a.mu.Unlock()
	return nil
}

func (a *Agent) beginStep() stepRuntime {
	step, _ := a.beginStepChecked()
	return step
}

// beginStepChecked applies one durable SettingsScheduled candidate before its
// binding becomes visible to a request. A failed SettingsChanged commit leaves
// the active binding untouched and returns the error to runTurn, which then
// stops before calling the provider.
func (a *Agent) beginStepChecked() (stepRuntime, error) {
	// Match applySetModel's transition lock. A pending candidate is validated,
	// durably applied, and published as one serialized transition; this avoids
	// writing an obsolete SettingsChanged fact when a replacement races the
	// boundary.
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()
	var lease *tools.Lease
	var mcpLease *mcp.StepLease
	for {
		var pending modelBinding
		var pendingRevision uint64
		var pendingChangeID string
		var capturedRevision uint64
		a.mu.Lock()
		if a.pendingBinding != nil {
			pending = *a.pendingBinding
			capturedRevision = a.pendingSettingsRevision
			pendingRevision = capturedRevision
			pendingChangeID = a.pendingSettingsChangeID
			if pendingRevision == 0 {
				pendingRevision = a.settingsRev + 1
				if pendingRevision == 0 {
					pendingRevision = 1
				}
			}
		}
		a.mu.Unlock()
		if pending.client == nil {
			break
		}
		a.mu.Lock()
		settings := a.sessionSettingsLocked()
		settings.Model = pending.model
		settings.Provider = pending.provider
		settings.Endpoint, _ = sanitizeRequestEndpoint(pending.endpoint)
		a.mu.Unlock()
		if err := a.persistScheduledSettingsAppliedFact(pendingChangeID, settings, pendingRevision); err != nil {
			return stepRuntime{}, err
		}
		a.mu.Lock()
		// A second model command can arrive while the first candidate is being
		// committed. Do not apply a stale binding; retry from a fresh snapshot
		// rather than recursing (which previously exhausted the process stack).
		candidateStillCurrent := a.pendingBinding != nil &&
			a.pendingBinding.model == pending.model &&
			a.pendingBinding.provider == pending.provider &&
			a.pendingBinding.endpoint == pending.endpoint &&
			a.pendingBinding.routeKind == pending.routeKind &&
			a.pendingSettingsRevision == capturedRevision &&
			a.pendingSettingsChangeID == pendingChangeID
		if !candidateStillCurrent {
			a.mu.Unlock()
			continue
		}
		a.applyBindingLocked(pending)
		a.pendingBinding = nil
		a.pendingSettingsRevision = 0
		a.pendingSettingsChangeID = ""
		a.settingsRev = pendingRevision
		a.mu.Unlock()
		break
	}
	a.mu.Lock()
	a.stepSeq++
	if a.mcp != nil {
		mcpLease = a.mcp.AcquireStep(a.registry)
		lease = mcpLease.Tools
	} else if a.registry != nil {
		// Capture implementations at the same boundary as cfg/provider/history.
		// The lease remains valid even if a plugin or MCP server replaces its
		// visible name while this request is streaming.
		lease = a.registry.Acquire()
	}
	step := stepRuntime{binding: a.activeBinding, cfg: cloneConfig(a.cfg), history: cloneMessages(a.history),
		turn: a.turnSeq, step: a.stepSeq, settingsRev: a.settingsRev, contextRev: a.contextRev,
		catalogVersion: a.catalogVersion, sessionID: a.sessionID, sessionStartAt: a.sessionStartAt,
		plan: a.planMode, modelSwitchMsg: a.modelSwitchMsg, interruptNote: a.interruptNote, toolLease: lease, mcpLease: mcpLease}
	// Capture all mutable parent inputs that a Task/guardian can inherit while
	// this step is executing. Keep the registry frozen at the same lock
	// boundary as cfg/binding; childOptionsFromParent verifies the turn before
	// consuming it, so a completed turn can never leak an old snapshot.
	var frozenModels *plugin.ModelRegistry
	if a.models != nil {
		frozenModels = a.models.Freeze()
	}
	var registryNames []string
	if lease != nil {
		registryNames = lease.Names()
	}
	a.childStep = &childStepSnapshot{
		mcpLease:      mcpLease,
		cfg:           cloneConfig(&step.cfg),
		binding:       step.binding,
		models:        frozenModels,
		registryNames: append([]string(nil), registryNames...),
		toolLease:     lease,
		preHooks: func() []plugin.PreToolHook {
			if a.gohooks == nil {
				return nil
			}
			return a.gohooks.SnapshotPreTool()
		}(),
		turn:     step.turn,
		revision: step.settingsRev,
	}
	a.modelSwitchMsg = ""
	a.interruptNote = false
	a.mu.Unlock()
	// Registry reads are taken once at the step boundary. A plugin/MCP update
	// during streaming therefore cannot mix a new schema with this step's
	// frozen model/config/history binding.
	// Instructions and the skill index are external mutable inputs too. Capture
	// their bounded snapshots at the same step boundary so a file edit during
	// provider preparation cannot change the request after admission.
	instructions, inputErr := workspace.LoadInstructionsChecked(step.cfg.Workspace)
	if inputErr != nil {
		releaseStepLease(step)
		a.mu.Lock()
		if a.childStep != nil && a.childStep.turn == step.turn && a.childStep.revision == step.settingsRev {
			a.childStep = nil
		}
		a.mu.Unlock()
		return stepRuntime{}, &stepInputError{err: fmt.Errorf("load project instructions: %w", inputErr)}
	}
	step.instructions = instructions
	a.mu.Lock()
	skillStore := a.skills
	isolated := a.childState != nil
	a.mu.Unlock()
	if skillStore != nil && !isolated {
		home, _ := os.UserHomeDir()
		if inputErr := skillStore.LoadChecked(filepath.Join(home, ".ccdp", "skills"), filepath.Join(step.cfg.Workspace, ".ccdp", "skills")); inputErr != nil {
			releaseStepLease(step)
			a.mu.Lock()
			if a.childStep != nil && a.childStep.turn == step.turn && a.childStep.revision == step.settingsRev {
				a.childStep = nil
			}
			a.mu.Unlock()
			return stepRuntime{}, &stepInputError{err: fmt.Errorf("load skills: %w", inputErr)}
		}
		step.skillsSection = skillStore.SkillsSection()
	}
	step.tools = a.toolDefsSnapshotFromLease(lease)
	return step, nil
}

type stepRuntime struct {
	binding        modelBinding
	cfg            config.Config
	history        []messages.Message
	turn           uint64
	step           uint64
	settingsRev    uint64
	contextRev     uint64
	catalogVersion uint64
	sessionID      string
	sessionStartAt time.Time
	plan           bool
	modelSwitchMsg string
	interruptNote  bool
	instructions   string
	skillsSection  string
	tools          []llm.ToolDef
	toolLease      *tools.Lease
	mcpLease       *mcp.StepLease
}

// stepInputError marks a bounded external prompt input (instructions, skill
// metadata, or a frozen attachment) that cannot be admitted for this request.
// It is a user-retryable input problem, not evidence that the session log is
// corrupt; callers must fail only this turn and leave the persistence gate
// usable for a subsequent fix/retry.
type stepInputError struct{ err error }

func (e *stepInputError) Error() string {
	if e == nil || e.err == nil {
		return "step input is unavailable"
	}
	return e.err.Error()
}

func (e *stepInputError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// buildRequestForStep forces the request model to the binding captured before
// context construction.
func (a *Agent) buildRequestForStep(step stepRuntime) llm.CompletionRequest {
	return a.buildRequestSnapshotWithPrompt(step.cfg, step.binding.model, step.history,
		step.sessionID, step.sessionStartAt, step.plan, step.modelSwitchMsg, step.interruptNote,
		step.instructions, step.skillsSection, step.tools)
}

// buildRequestForStepChecked verifies every frozen image before constructing a
// provider request. A canonical input that references a missing/corrupt blob
// is a durable-integrity failure, not a reason to silently send the markdown
// marker and pretend the model saw the image.
func (a *Agent) buildRequestForStepChecked(step stepRuntime) (llm.CompletionRequest, error) {
	var imageBytes int64
	for _, message := range step.history {
		if message.Role != messages.RoleUser || !message.ImagesFrozen {
			continue
		}
		_, used, _, err := a.imageContentPartsForMessage(step.cfg.Workspace, message, maxAttachmentBytes-imageBytes)
		if err != nil {
			return llm.CompletionRequest{}, &stepInputError{err: fmt.Errorf("frozen input image: %w", err)}
		}
		imageBytes += used
	}
	return a.buildRequestForStep(step), nil
}

// toolDefsSnapshot converts one registry view into provider-neutral wire
// declarations. The discovered/deferred decision is copied before reading the
// registry so the callback never re-enters Agent.mu.
func (a *Agent) toolDefsSnapshot() []llm.ToolDef {
	if a.registry == nil {
		return nil
	}
	return a.toolDefsSnapshotFromLease(a.registry.Acquire())
}

func (a *Agent) toolDefsSnapshotFromLease(lease *tools.Lease) []llm.ToolDef {
	if lease == nil {
		return nil
	}
	a.mu.Lock()
	deferred := make(map[string]bool, len(a.deferTools))
	for name, value := range a.deferTools {
		deferred[name] = value
	}
	discovered := make(map[string]bool, len(a.discovered))
	for name, value := range a.discovered {
		discovered[name] = value
	}
	a.mu.Unlock()
	schemas := lease.SchemasFiltered(func(name string) bool {
		return !deferred[name] || discovered[name]
	})
	defs := make([]llm.ToolDef, 0, len(schemas))
	for _, schema := range schemas {
		fn, ok := schema["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		description, _ := fn["description"].(string)
		parameters, _ := fn["parameters"].(map[string]any)
		if name == "" {
			continue
		}
		tool, _ := lease.Get(name)
		defs = append(defs, llm.ToolDef{Type: "function", PlanAllowed: planAllowedImplementation(tool), Function: llm.FuncDef{
			Name: name, Description: description, Parameters: cloneMap(parameters),
		}})
	}
	return defs
}

func (a *Agent) applyExecutionMode(cmd protocol.Command) protocol.Receipt {
	if a.isBusy() {
		return a.rejectedReceipt(cmd, protocol.ErrorBusy, "execution mode changes apply only between steps")
	}
	on := cmd.ExecutionMode.Mode == protocol.ExecutionModePlan
	if err := a.setExecutionMode(on); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
}

func (a *Agent) setExecutionMode(on bool) error {
	a.mu.Lock()
	base := a.planBaseMode
	if on {
		if !a.planMode {
			a.planBaseMode = a.perms.CurrentMode()
			if a.planBaseMode == permissions.ModePlan {
				a.planBaseMode = permissions.ModeDefault
			}
		}
		base = a.planBaseMode
		// The candidate is persisted before any public mode/workflow mutation.
	} else {
		if base == "" {
			base = permissions.ModeDefault
		}
	}
	nextRevision := a.settingsRev + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	settings := a.sessionSettingsLocked()
	if on {
		settings.ExecutionMode = "plan"
	} else {
		settings.ExecutionMode = "execute"
	}
	settings.PermissionPolicy = string(base)
	workflow := session.WorkflowState{Phase: string(protocol.WorkflowOff)}
	if on {
		workflow.Phase = string(protocol.WorkflowDrafting)
	}
	a.mu.Unlock()
	if err := a.persistSettingsWorkflowFact(settings, nextRevision, &workflow); err != nil {
		return err
	}
	a.mu.Lock()
	if on {
		if !a.planMode {
			a.planBaseMode = base
		}
		a.planMode = true
		a.workflow = protocol.WorkflowDrafting
	} else {
		a.planMode = false
		a.workflow = protocol.WorkflowOff
		a.planBaseMode = base
	}
	a.settingsRev = nextRevision
	a.cfg.PermissionMode = string(base)
	a.baseCfg.PermissionMode = string(base)
	a.mu.Unlock()
	a.perms.SetMode(base)
	a.syncHookContext()
	a.emit(Event{Type: EventPlanModeChanged, PlanMode: on})
	a.emitStatus("plan mode %s", map[bool]string{true: "on", false: "off"}[on])
	a.publishState()
	return nil
}

func (a *Agent) applyPermissionPolicy(cmd protocol.Command) protocol.Receipt {
	if a.isBusy() {
		return a.rejectedReceipt(cmd, protocol.ErrorBusy, "permission changes apply only after the current turn settles")
	}
	p := cmd.PermissionPolicy.Policy
	a.mu.Lock()
	currentMode := a.perms.CurrentMode()
	currentAllow := append([]string(nil), a.cfg.AlwaysAllow...)
	currentDeny := append([]string(nil), a.cfg.AlwaysDeny...)
	plan := a.planMode
	a.mu.Unlock()
	mode := currentMode
	var err error
	if p.Mode != "" {
		mode, err = permissions.ParseMode(p.Mode)
		if err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
		}
	}
	if mode == permissions.ModePlan {
		// /mode plan is a compatibility entry into the independent plan
		// workflow. Preserve the current policy, which may have changed since
		// planBaseMode was captured at startup or on a previous plan entry.
		mode = currentMode
		if mode == permissions.ModePlan || mode == "" {
			mode = permissions.ModeDefault
		}
		plan = true
	}
	if p.AlwaysAllow != nil {
		currentAllow = append([]string(nil), p.AlwaysAllow...)
	}
	if p.AlwaysDeny != nil {
		currentDeny = append([]string(nil), p.AlwaysDeny...)
	}
	a.mu.Lock()
	nextRevision := a.settingsRev + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	settings := a.sessionSettingsLocked()
	settings.PermissionPolicy = string(mode)
	settings.AlwaysAllow = append([]string(nil), currentAllow...)
	settings.AlwaysDeny = append([]string(nil), currentDeny...)
	settings.ExecutionMode = "execute"
	workflow := session.WorkflowState{Phase: string(protocol.WorkflowOff)}
	if plan {
		settings.ExecutionMode = "plan"
		workflow.Phase = string(protocol.WorkflowDrafting)
	}
	a.mu.Unlock()
	if err := a.persistSettingsWorkflowFact(settings, nextRevision, &workflow); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	a.mu.Lock()
	a.cfg.AlwaysAllow = append([]string(nil), currentAllow...)
	a.cfg.AlwaysDeny = append([]string(nil), currentDeny...)
	a.baseCfg.AlwaysAllow = append([]string(nil), currentAllow...)
	a.baseCfg.AlwaysDeny = append([]string(nil), currentDeny...)
	if plan {
		a.planBaseMode = mode
		a.planMode = true
		a.workflow = protocol.WorkflowDrafting
	} else {
		a.planMode = false
		a.workflow = protocol.WorkflowOff
	}
	a.cfg.PermissionMode = string(mode)
	a.baseCfg.PermissionMode = string(mode)
	a.settingsRev = nextRevision
	a.mu.Unlock()
	a.perms.SetMode(mode)
	a.perms.SetPolicy(permissions.Policy{AlwaysAllow: append([]string(nil), currentAllow...), AlwaysDeny: append([]string(nil), currentDeny...)})
	a.syncHookContext()
	a.emit(Event{Type: EventModeChanged, Mode: mode})
	a.emitStatus("permission policy updated")
	a.publishState()
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
}

func (a *Agent) applySandboxPolicy(cmd protocol.Command) protocol.Receipt {
	if a.isBusy() {
		return a.rejectedReceipt(cmd, protocol.ErrorBusy, "sandbox changes apply only after the current turn settles")
	}
	p := cmd.SandboxPolicy.Policy
	mode, err := sandbox.ParseMode(p.Mode)
	if err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	a.mu.Lock()
	cfg := cloneConfig(a.cfg)
	cfg.SandboxMode = string(mode)
	// Nil directory lists denote a mode-only patch. Preserve the existing
	// network and directory policy for that form; a non-nil list is an explicit
	// replacement (including an intentionally empty list).
	if p.AdditionalDirectories != nil || p.DisallowedDirectories != nil {
		cfg.SandboxAllowNetwork = p.AllowNetwork
		if p.AdditionalDirectories != nil {
			cfg.AdditionalDirectories = append([]string(nil), p.AdditionalDirectories...)
		}
		if p.DisallowedDirectories != nil {
			cfg.DisallowedDirectories = append([]string(nil), p.DisallowedDirectories...)
		}
	}
	nextRevision := a.settingsRev + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	settings := a.sessionSettingsLocked()
	settings.SandboxPolicy = cfg.SandboxMode
	settings.AllowNetwork = cfg.SandboxAllowNetwork
	settings.AdditionalDirectories = append([]string(nil), cfg.AdditionalDirectories...)
	settings.DisallowedDirectories = append([]string(nil), cfg.DisallowedDirectories...)
	a.mu.Unlock()
	if err := a.persistSettingsFact(settings, nextRevision); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	a.mu.Lock()
	a.cfg.SandboxMode = cfg.SandboxMode
	a.cfg.SandboxAllowNetwork = cfg.SandboxAllowNetwork
	a.cfg.AdditionalDirectories = append([]string(nil), cfg.AdditionalDirectories...)
	a.cfg.DisallowedDirectories = append([]string(nil), cfg.DisallowedDirectories...)
	a.baseCfg.SandboxMode = cfg.SandboxMode
	a.baseCfg.SandboxAllowNetwork = cfg.SandboxAllowNetwork
	a.baseCfg.AdditionalDirectories = append([]string(nil), cfg.AdditionalDirectories...)
	a.baseCfg.DisallowedDirectories = append([]string(nil), cfg.DisallowedDirectories...)
	a.sandbox = buildSandbox(&cfg, cfg.Workspace)
	a.settingsRev = nextRevision
	a.mu.Unlock()
	a.emit(Event{Type: EventSandboxChanged})
	a.emitStatus("sandbox mode → %s", mode)
	a.publishState()
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
}

func (a *Agent) applyApproveTool(cmd protocol.Command) protocol.Receipt {
	a.mu.Lock()
	resp := a.approvalResp
	pending := a.pendingApproval
	if resp == nil || pending == nil || pending.ID != cmd.Approval.ApprovalID {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorNotFound, "approval is no longer pending")
	}
	if a.approvalResolving {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "approval response was already delivered")
	}
	a.approvalResolving = true
	decision := "deny"
	if cmd.Approval.Approve {
		decision = "allow"
	}
	approvalID := pending.journalID
	if approvalID == "" {
		approvalID = cmd.Approval.ApprovalID
	}
	reason := pending.Reason
	a.mu.Unlock()
	if err := a.persistApprovalResolved(session.ApprovalResolution{
		ApprovalID: approvalID,
		Decision:   decision,
		Reason:     reason,
		ResolvedBy: "user",
	}); err != nil {
		// Wake the waiting tool without asking it to write a second resolution.
		// The persistence gate remains failed, so the tool cannot cross its
		// side-effect boundary.
		select {
		case resp <- approvalAnswer{persisted: true}:
		default:
		}
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	select {
	case resp <- approvalAnswer{approve: cmd.Approval.Approve, remember: cmd.Approval.Remember, persisted: true}:
		a.publishState()
		return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
	default:
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "approval response was already delivered")
	}
}

func (a *Agent) applyApprovePlan(cmd protocol.Command) protocol.Receipt {
	a.mu.Lock()
	resp := a.planResp
	pending := a.pendingPlan
	if pending == nil || resp == nil || pending.ID != cmd.Plan.PlanID {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorNotFound, "plan decision is no longer pending")
	}
	if cmd.Plan.PlanVersion != 0 && cmd.Plan.PlanVersion != planViewVersion {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorStaleRevision, "plan version is stale")
	}
	a.mu.Unlock()
	if cmd.Plan.Approve {
		if err := a.setExecutionMode(false); err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
		}
	} else {
		workflow := session.WorkflowState{Phase: string(protocol.WorkflowDrafting), PlanID: cmd.Plan.PlanID, PlanVersion: cmd.Plan.PlanVersion}
		if err := a.persistWorkflowFact(workflow); err != nil {
			return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
		}
		a.mu.Lock()
		a.workflow = protocol.WorkflowDrafting
		a.mu.Unlock()
		a.publishState()
	}
	select {
	case resp <- cmd.Plan.Approve:
		a.publishState()
		return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
	default:
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidState, "plan response was already delivered")
	}
}

// Snapshot returns a deep, credential-free view of runtime state.
func (a *Agent) Snapshot(ctx context.Context) (protocol.SessionView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return protocol.SessionView{}, ctx.Err()
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked(), nil
}

func (a *Agent) snapshotLocked() protocol.SessionView {
	settings := a.settingsSnapshotLocked()
	view := protocol.SessionView{SessionID: protocol.SessionID(a.sessionID), Revision: a.revisionLocked(),
		Phase: a.phase, Workflow: a.workflow, Busy: a.busy, Closing: a.closing,
		Settings: settings, Catalog: a.catalogSnapshotLocked(), Usage: protocol.UsageSnapshot{
			InputTokens: a.usage.InputTokens, OutputTokens: a.usage.OutputTokens,
			CachedTokens: a.usage.CachedTokens, Cost: a.usage.Cost, TurnCount: a.usage.TurnCount,
		}}
	if a.pendingBinding != nil {
		p := a.pendingSettingsLocked()
		view.Pending = &p
	}
	view.History = make([]protocol.MessageView, 0, len(a.history))
	for _, m := range a.history {
		mv := protocol.MessageView{ID: m.ID, Role: string(m.Role), Content: m.Content}
		for _, tc := range m.ToolCalls {
			mv.ToolCallIDs = append(mv.ToolCallIDs, protocol.CallID(tc.ID))
		}
		view.History = append(view.History, mv)
	}
	view.PendingInputs = append([]protocol.InputView(nil), a.pendingInputs...)
	if a.pendingApproval != nil {
		view.Approval = &protocol.ApprovalView{ID: a.pendingApproval.ID, Tool: a.pendingApproval.Tool,
			Reason: a.pendingApproval.Reason, Args: approvalArgsView(a.pendingApproval)}
	}
	view.Question = protocol.CloneQuestionRequest(a.pendingQuestion)
	if a.pendingPlan != nil {
		view.Plan = &protocol.PlanView{ID: a.pendingPlan.ID, Text: a.pendingPlan.Plan, Version: planViewVersion}
	}
	if a.lastTurn != nil {
		outcome := *a.lastTurn
		view.LastTurn = &outcome
	}
	view.ContextUsedTokens = a.contextUsedTokensLocked()
	view.Transcript, view.TranscriptMore = a.transcript.snapshot()
	return view
}

// contextUsedTokensLocked mirrors estimateTokens while retaining the snapshot
// lock. Calling estimateTokens here would try to acquire a.mu recursively and
// would make Snapshot deadlock.
func (a *Agent) contextUsedTokensLocked() int {
	base, baseLen := a.tokenBaseline.promptTokens, a.tokenBaseline.historyLen
	if base > 0 && baseLen <= len(a.history) {
		for _, m := range a.history[baseLen:] {
			base += estimateMessageTokens(m)
		}
		return base
	}
	for _, m := range a.history {
		base += estimateMessageTokens(m)
	}
	return base
}

// approvalArgsView keeps the new Watch/Snapshot DTO credential-free even
// though the compatibility ApprovalRequest currently stores only its display
// command. If that command is already JSON (newer producers), preserve the
// object bytes; otherwise expose a stable command field for older producers.
func approvalArgsView(req *ApprovalRequest) json.RawMessage {
	if req == nil || req.Command == "" {
		return nil
	}
	if json.Valid([]byte(req.Command)) {
		return append(json.RawMessage(nil), req.Command...)
	}
	b, err := json.Marshal(map[string]string{"command": req.Command})
	if err != nil {
		return nil
	}
	return b
}

func (a *Agent) settingsSnapshotLocked() protocol.SettingsSnapshot {
	b := a.activeBinding
	return protocol.SettingsSnapshot{Revision: a.settingsRev,
		Model:           protocol.ModelBinding{Model: b.model, Provider: b.provider, Endpoint: b.endpoint, BindingVersion: b.version},
		ExecutionMode:   map[bool]protocol.ExecutionMode{true: protocol.ExecutionModePlan, false: protocol.ExecutionModeExecute}[a.planMode],
		Permission:      protocol.PermissionPolicy{Mode: string(a.perms.CurrentMode()), AlwaysAllow: append([]string(nil), a.cfg.AlwaysAllow...), AlwaysDeny: append([]string(nil), a.cfg.AlwaysDeny...), Revision: a.settingsRev},
		Sandbox:         protocol.SandboxPolicy{Mode: a.cfg.SandboxMode, AllowNetwork: a.cfg.SandboxAllowNetwork, AdditionalDirectories: append([]string(nil), a.cfg.AdditionalDirectories...), DisallowedDirectories: append([]string(nil), a.cfg.DisallowedDirectories...), Revision: a.settingsRev},
		ReasoningEffort: a.cfg.ReasoningEffort, Verbosity: a.cfg.Verbosity,
		ContextWindow: a.cfg.ContextWindow, MaxReplyTokens: a.cfg.MaxReplyTokens, MaxTurns: a.cfg.MaxTurns, MaxBudgetUSD: a.cfg.MaxBudgetUSD}
}

func (a *Agent) pendingSettingsLocked() protocol.PendingSettings {
	p := protocol.PendingSettings{Revision: a.settingsRev}
	if a.pendingBinding != nil {
		if a.pendingSettingsRevision > p.Revision {
			p.Revision = a.pendingSettingsRevision
		}
		m := protocol.ModelBinding{Model: a.pendingBinding.model, Provider: a.pendingBinding.provider, Endpoint: a.pendingBinding.endpoint, BindingVersion: a.pendingBinding.version}
		p.Model = &m
	}
	return p
}

func (a *Agent) catalogSnapshotLocked() protocol.CatalogSnapshot {
	providers := a.models.Names()
	out := protocol.CatalogSnapshot{Version: a.catalogVersion}
	for _, p := range providers {
		out.Providers = append(out.Providers, protocol.ProviderSnapshot{ID: p, Version: a.catalogVersion})
	}
	for _, name := range a.registry.Names() {
		t, ok := a.registry.Get(name)
		if !ok {
			continue
		}
		out.Tools = append(out.Tools, protocol.ToolSnapshot{ID: t.Name(), Schema: marshalSchema(t.Parameters())})
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].ID < out.Providers[j].ID })
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].ID < out.Tools[j].ID })
	return out
}

func marshalSchema(v map[string]any) []byte {
	b, _ := jsonMarshal(v)
	return b
}

// jsonMarshal is a variable-sized helper kept here so snapshot construction
// never stores a map owned by the registry. (The actual encoding is local.)
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func (a *Agent) publishState() {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	a.mu.Lock()
	view := a.snapshotLocked()
	rev := view.Revision
	a.mu.Unlock()
	update := protocol.Update{Type: protocol.UpdateState, Cursor: protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration}, Revision: rev, Snapshot: &view}
	a.stampUpdateLocked(&update)
	for _, watcher := range a.watchers {
		a.sendWatcherLocked(watcher, update)
	}
}

// publishTerminal publishes the final idle snapshot and its TurnDone event as
// one watcher-ordered boundary. The watcher lock is acquired before the owner
// lock, matching publishState and preventing a command from observing the
// state between the terminal snapshot and the event. If a queued input was
// already admitted, a busy reservation for its next turn is published after
// TurnDone and the caller must drain it. No eventView call is made here: it
// would re-enter a.mu and a second, delayed conversion could broadcast an old
// TurnDone after a new user message has started.
func (a *Agent) publishTerminal(turnID protocol.TurnID) bool {
	a.watchMu.Lock()
	a.mu.Lock()
	// turnFinished established the terminal outcome before entering this
	// helper. Reassert the externally visible idle boundary while the locks are
	// held, then freeze the exact snapshot sent ahead of TurnDone.
	a.busy = false
	if a.closing || a.closed {
		a.phase = protocol.PhaseStopping
	} else {
		a.phase = protocol.PhaseIdle
	}
	a.settling = false
	a.ensureTypedPendingLocked()
	canDrain := !a.closing && !a.closed && !a.stop && !a.interruptFlag && a.persistenceErr == nil && len(a.pendingInputs) > 0
	view := a.snapshotLocked()
	rev := view.Revision
	done := protocol.EventView{
		Kind:           protocol.EventTurnDone,
		SessionID:      protocol.SessionID(a.sessionID),
		TurnID:         turnID,
		StepID:         protocol.StepID(fmt.Sprintf("%d", a.stepSeq)),
		Phase:          a.phase,
		Workflow:       a.workflow,
		PermissionMode: string(a.perms.CurrentMode()),
	}
	state := protocol.Update{Type: protocol.UpdateState, Cursor: protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration}, Revision: rev, Snapshot: &view}
	terminal := protocol.Update{Type: protocol.UpdateStream, Cursor: protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration}, Revision: rev, Event: &done, Text: done.Text}
	a.stampUpdateLocked(&state)
	a.stampUpdateLocked(&terminal)
	for _, watcher := range a.watchers {
		a.sendWatcherLocked(watcher, state)
		a.sendWatcherLocked(watcher, terminal)
	}
	if canDrain {
		// Reserve the next turn before releasing the owner lock. A Submit that
		// arrives after TurnDone therefore queues behind this handoff instead of
		// racing the old turn's claim and starting a second turn.
		a.busy = true
		a.phase = protocol.PhasePreparing
		next := a.snapshotLocked()
		nextRev := next.Revision
		nextState := protocol.Update{Type: protocol.UpdateState, Cursor: protocol.Cursor{LogSeq: nextRev.LogSeq, ViewGeneration: nextRev.ViewGeneration}, Revision: nextRev, Snapshot: &next}
		a.stampUpdateLocked(&nextState)
		for _, watcher := range a.watchers {
			a.sendWatcherLocked(watcher, nextState)
		}
	}
	a.mu.Unlock()
	a.watchMu.Unlock()
	return canDrain
}

func (a *Agent) publishReceipt(receipt protocol.Receipt) {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	update := protocol.Update{Type: protocol.UpdateReceipt, Cursor: protocol.Cursor{LogSeq: receipt.Revision.LogSeq, ViewGeneration: receipt.Revision.ViewGeneration}, Revision: receipt.Revision, Receipt: &receipt}
	a.stampUpdateLocked(&update)
	for _, watcher := range a.watchers {
		a.sendWatcherLocked(watcher, update)
	}
}

func (a *Agent) stampUpdateLocked(update *protocol.Update) {
	a.eventSeq++
	update.Cursor.StreamEpoch, update.Cursor.EventSeq = a.streamEpoch, a.eventSeq
}

func (a *Agent) publishEvent(ev Event) {
	view := a.eventView(ev)
	a.mu.Lock()
	rev := a.revisionLocked()
	a.mu.Unlock()
	update := protocol.Update{Type: protocol.UpdateStream,
		Cursor:   protocol.Cursor{LogSeq: rev.LogSeq, ViewGeneration: rev.ViewGeneration},
		Revision: rev, Event: &view, Text: view.Text}
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	a.stampUpdateLocked(&update)
	a.transcript.event(view)
	view.Transcript = a.transcript.eventItem(view)
	for _, watcher := range a.watchers {
		a.sendWatcherLocked(watcher, update)
	}
}

// eventView is the only bridge from the legacy Event channel to the typed
// Watch protocol. The old channel remains available to existing adapters, but
// new clients never need to inspect agent-owned pointers or enums.
func (a *Agent) eventView(ev Event) protocol.EventView {
	a.mu.Lock()
	turnID := protocol.TurnID(ev.TurnID)
	if turnID == "" {
		turnID = protocol.TurnID(fmt.Sprintf("%d", a.turnSeq))
	}
	a.observeTurnEventLocked(ev, turnID)
	view := protocol.EventView{SessionID: protocol.SessionID(a.sessionID), TurnID: turnID, StepID: protocol.StepID(fmt.Sprintf("%d", a.stepSeq)), Phase: a.phase, Workflow: a.workflow, MessageID: ev.MessageID, Text: ev.Text}
	mode := a.perms.CurrentMode()
	a.mu.Unlock()
	view.PermissionMode = string(mode)
	switch ev.Type {
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
			Reason: ev.Approval.Reason, Args: approvalArgsView(ev.Approval)}
		view.CallID = protocol.CallID(ev.Approval.ID)
	}
	if ev.Plan != nil {
		view.Plan = &protocol.PlanView{ID: ev.Plan.ID, Text: ev.Plan.Plan, Version: planViewVersion}
	}
	if ev.Usage != nil {
		view.Usage = &protocol.UsageSnapshot{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens, CachedTokens: ev.Usage.CachedTokens, Cost: ev.Usage.Cost, TurnCount: ev.Usage.TurnCount}
	}
	return view
}

// observeTurnEventLocked keeps a small recovery projection alongside the
// transient event stream. It intentionally keys updates off a running turn,
// rather than merely EventError, so diagnostics from commands and operations
// (for example a failed fork) cannot change the latest turn outcome.
func (a *Agent) observeTurnEventLocked(ev Event, turnID protocol.TurnID) {
	if a.turnSeq == 0 {
		return
	}
	sameTurn := a.lastTurn != nil && a.lastTurn.TurnID == turnID
	if !a.busy && ev.Type != EventTurnDone {
		return
	}
	switch ev.Type {
	case EventUserMsg:
		if !sameTurn {
			a.lastTurn = &protocol.TurnOutcome{TurnID: turnID, Status: protocol.TurnRunning}
		}
	case EventError:
		// Only a turn that announced its user input can own an error. This
		// excludes command/operation diagnostics even when a turn is busy.
		if sameTurn && a.lastTurn.Status == protocol.TurnRunning {
			a.lastTurn.Status = protocol.TurnFailed
			a.lastTurn.Error = ev.Text
		}
	case EventTurnDone:
		if !sameTurn || (a.lastTurn.Status != protocol.TurnRunning && a.lastTurn.Status != protocol.TurnFailed) {
			return
		}
		if a.lastTurn.Status == protocol.TurnFailed {
			return
		}
		if a.interruptFlag || a.stop || a.closing || a.closed {
			a.lastTurn.Status = protocol.TurnCancelled
		} else {
			a.lastTurn.Status = protocol.TurnSucceeded
		}
	}
}

// sendWatcherLocked reserves one channel slot for a resync marker. Once a
// consumer falls behind, no later update can silently erase the recovery
// signal; the terminal subscription tells it to Snapshot and re-Watch.
func (a *Agent) sendWatcherLocked(w *runtimeWatcher, update protocol.Update) {
	if w == nil || w.resync {
		return
	}
	select {
	case <-w.closed:
		delete(a.watchers, w.key)
		return
	default:
	}
	if cap(w.ch) <= 1 || len(w.ch) < cap(w.ch)-1 {
		select {
		case w.ch <- update:
		default:
		}
		return
	}
	for len(w.ch) >= cap(w.ch) {
		<-w.ch
	}
	rev := update.Revision
	w.ch <- protocol.Update{Type: protocol.UpdateResyncRequired,
		Cursor: update.Cursor, Revision: rev}
	w.resync = true
	// A resync marker is terminal for this subscription. The client must take
	// a fresh Snapshot and establish a new Watch atomically; retaining a
	// half-consumed stream would make cursor semantics ambiguous.
	a.closeWatcherLocked(w)
}

func (a *Agent) closeWatchers() {
	a.watchMu.Lock()
	defer a.watchMu.Unlock()
	for key, watcher := range a.watchers {
		a.closeWatcherLocked(watcher)
		delete(a.watchers, key)
	}
}

// closeWatcherLocked is called only while owner.watchMu is held. Keeping
// closure and publication under one lock avoids an ABBA race between a
// consumer's Close and runtime shutdown.
func (a *Agent) closeWatcherLocked(w *runtimeWatcher) {
	if w == nil {
		return
	}
	select {
	case <-w.closed:
		return
	default:
	}
	close(w.closed)
	close(w.ch)
	delete(a.watchers, w.key)
}

func (a *Agent) Watch(ctx context.Context, cursor protocol.Cursor) (protocol.Subscription, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	w := &runtimeWatcher{owner: a, ch: make(chan protocol.Update, 64), closed: make(chan struct{})}
	a.watchMu.Lock()
	a.mu.Lock()
	if a.closed || a.closing {
		a.mu.Unlock()
		a.watchMu.Unlock()
		return nil, errors.New("agent: session is closed")
	}
	view := a.snapshotLocked()
	a.nextWatch++
	w.key = a.nextWatch
	a.watchers[w.key] = w
	a.mu.Unlock()
	a.sendWatcherLocked(w, protocol.Update{Type: protocol.UpdateSnapshot, Cursor: protocol.Cursor{LogSeq: view.Revision.LogSeq, ViewGeneration: view.Revision.ViewGeneration, StreamEpoch: a.streamEpoch, EventSeq: a.eventSeq}, Revision: view.Revision, Snapshot: &view})
	a.watchMu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			_ = w.Close()
		case <-a.rootCtx.Done():
			_ = w.Close()
		case <-w.closed:
		}
	}()
	_ = cursor // updates are state-based; a resync snapshot is always safe.
	return w, nil
}

func (w *runtimeWatcher) Updates() <-chan protocol.Update { return w.ch }
func (w *runtimeWatcher) Close() error {
	w.owner.watchMu.Lock()
	w.owner.closeWatcherLocked(w)
	w.owner.watchMu.Unlock()
	return nil
}

// cloneConfig deep-copies mutable maps/slices and intentionally keeps no
// secret-bearing runtime pointers. It is used for step and model binding
// snapshots so /reload cannot mutate a request in flight.
func cloneConfig(src *config.Config) config.Config {
	if src == nil {
		return config.Config{}
	}
	return src.Clone()
}

func (a *Agent) configSnapshot() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneConfig(a.cfg)
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneMessages(src []messages.Message) []messages.Message {
	dst := make([]messages.Message, len(src))
	for i, m := range src {
		dst[i] = m
		dst[i].ToolCalls = append([]messages.ToolCall(nil), m.ToolCalls...)
		dst[i].Content = m.Content
		if m.ImageAttachments != nil {
			dst[i].ImageAttachments = make([]messages.ImageAttachment, len(m.ImageAttachments))
			for j, attachment := range m.ImageAttachments {
				dst[i].ImageAttachments[j] = attachment
				dst[i].ImageAttachments[j].Data = append([]byte(nil), attachment.Data...)
			}
		}
		for j := range dst[i].ToolCalls {
			dst[i].ToolCalls[j].Arguments = cloneMap(m.ToolCalls[j].Arguments)
		}
	}
	return dst
}

var _ protocol.SessionClient = (*Agent)(nil)
var _ protocol.Subscription = (*runtimeWatcher)(nil)
