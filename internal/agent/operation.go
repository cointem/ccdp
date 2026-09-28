package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"context"
	"fmt"
	"time"
)

type operationMode uint8

const (
	operationExclusive operationMode = iota
	operationConcurrent
)

type commandOperation func(context.Context) (string, error)

func (a *Agent) scheduleCommandOperation(cmd protocol.Command, name string, operation commandOperation) protocol.Receipt {
	return a.scheduleOperation(cmd, name, operation, operationExclusive)
}

// scheduleOperation runs a typed operation with one durable
// completion/receipt boundary. Read-only queries and an explicit Save may run
// alongside a model turn; mutating operations reserve the session busy state.
func (a *Agent) scheduleOperation(cmd protocol.Command, name string, operation commandOperation, mode operationMode) protocol.Receipt {
	requireIdle := mode == operationExclusive
	a.mu.Lock()
	revoking := a.capabilityRevoking
	if (requireIdle && (a.busy || a.settling)) || a.closing || a.closed ||
		revoking && name != "query:doctor" && name != "query:status" {
		a.mu.Unlock()
		if revoking {
			return a.rejectedReceipt(cmd, protocol.ErrorBusy, "sandbox capability revocation is in progress")
		}
		if requireIdle {
			return a.rejectedReceipt(cmd, protocol.ErrorBusy, "command requires an idle session")
		}
		return a.rejectedReceipt(cmd, protocol.ErrorClosed, "session is closing")
	}
	ctx, cancel := context.WithCancel(a.rootCtx)
	if requireIdle {
		a.busy = true
		a.workStartedAt = time.Now()
		a.workLabel = name
		a.interruptFlag = false
		a.stop = false
		a.phase = protocol.PhaseExecutingTools
		if cmd.Type == protocol.CommandCompact {
			a.phase = protocol.PhaseCompacting
		}
		a.turnCancel = cancel
		a.turnCtx = ctx
	}
	a.operationWG.Add(1)
	trackForSandboxQuiescence := !sandboxQuiescenceOwner(name) && name != "query:doctor" && name != "query:status"
	if trackForSandboxQuiescence {
		a.sandboxOperationWG.Add(1)
	}
	a.mu.Unlock()
	if err := a.persistCommandScheduled(cmd, name, a.revision().LogSeq); err != nil {
		if requireIdle {
			a.mu.Lock()
			if a.turnCtx == ctx {
				a.turnCancel = nil
				a.turnCtx = nil
				a.busy = false
				a.phase = protocol.PhaseIdle
			}
			a.mu.Unlock()
		}
		cancel()
		a.operationWG.Done()
		if trackForSandboxQuiescence {
			a.sandboxOperationWG.Done()
		}
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	if requireIdle {
		a.publishState()
	}
	receipt := a.receipt(cmd, protocol.ReceiptScheduled, protocol.OperationID(cmd.ID), nil)
	go a.completeScheduledOperation(ctx, cancel, cmd, name, operation, requireIdle, trackForSandboxQuiescence)
	return receipt
}

// completeScheduledOperation runs a long-lived command operation, releases its
// busy reservation, and persists the terminal CommandCompleted admission ahead
// of the final receipt so a restart cannot observe a success without it.
func (a *Agent) completeScheduledOperation(ctx context.Context, cancel context.CancelFunc, cmd protocol.Command, name string, operation func(context.Context) (string, error), requireIdle, trackForSandboxQuiescence bool) {
	defer a.operationWG.Done()
	if trackForSandboxQuiescence {
		defer a.sandboxOperationWG.Done()
	}
	output, err := operation(ctx)
	if err == nil {
		err = ctx.Err()
	}
	status := "success"
	if err != nil {
		status = "error"
		if name == "review" && output == "" {
			output = "review: " + err.Error()
		}
	}
	boundedOutput := boundedCommandOutput(output)
	// receipt persists CommandCompleted before publishing the final receipt.
	// Keep that durable boundary ahead of all transient operation output so a
	// restart cannot observe a successful report without its completion fact.
	completion := &session.CommandCompleted{CommandID: string(cmd.ID), Outcome: status}
	if err != nil {
		completion.Code = string(protocol.ErrorInternal)
		completion.Report = err.Error()
	}
	var owner context.Context
	if requireIdle {
		owner = ctx
	}
	receiptStatus := protocol.ReceiptApplied
	var commandErr *protocol.CommandError
	if err != nil {
		receiptStatus = protocol.ReceiptRejected
		commandErr = &protocol.CommandError{Code: protocol.ErrorInternal, Message: err.Error()}
	}
	seq := a.currentTurnSeq()
	finalReceipt, drain := a.completeCommand(cmd, receiptStatus, protocol.OperationID(cmd.ID), commandErr, completion, output, owner)
	if finalReceipt.OperationID == "" {
		// Persistence failures clear OperationID; operation failures retain it
		// and still carry a durable report that clients must receive.
		reason := "command completion was not persisted"
		if finalReceipt.Error != nil {
			reason = finalReceipt.Error.Error()
		}
		a.emit(Event{Type: EventError, Text: name + ": " + reason})
	} else {
		a.emit(Event{Type: EventToolResult, Tool: &ToolEvent{ID: string(cmd.ID), Name: name, Status: status, Output: boundedOutput}})
		if err != nil && name != "review" {
			a.emit(Event{Type: EventError, Text: name + ": " + err.Error()})
		}
	}
	cancel()
	if drain {
		a.launchQueuedTurn(seq)
	}
}

func (a *Agent) scheduleCompact(cmd protocol.Command) protocol.Receipt {
	return a.scheduleCommandOperation(cmd, "compact", func(ctx context.Context) (string, error) {
		if err := a.compactContext(ctx); err != nil {
			return "", err
		}
		return "compaction complete", nil
	})
}
func (a *Agent) scheduleFork(cmd protocol.Command) protocol.Receipt {
	return a.scheduleOperation(cmd, "fork", func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		id, err := a.Fork(cmd.Fork.Count)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("forked session %s", id), nil
	}, operationConcurrent)
}
