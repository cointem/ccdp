package agent

import (
	"ccdp/internal/protocol"
	"context"
)

type controlCommand struct{ request runtimeCommand }

func (controlCommand) ownerControlMessage() {}
func isControlCommand(kind protocol.CommandType) bool {
	switch kind {
	case protocol.CommandInterrupt, protocol.CommandStop, protocol.CommandApproveTool, protocol.CommandApprovePlan, protocol.CommandAnswerQuestion:
		return true
	}
	return false
}

type rejectedCommand struct {
	receipt protocol.Receipt
	digest  string
}

// Pre-admission failures have no durable effect. The caller captures a receipt
// into a producer mailbox; only the owner merges it into command identity state.
func (a *Agent) rejectBeforeAdmission(cmd protocol.Command, code protocol.ErrorCode, text string) protocol.Receipt {
	if a.owner == nil {
		return a.rejectedReceipt(cmd, code, text)
	}
	digest, err := commandInputDigest(cmd)
	receipt := protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: protocol.ReceiptRejected, Revision: a.revision(), Error: &protocol.CommandError{Code: code, Message: text}}
	if cmd.ID == "" || err != nil {
		return receipt
	}
	a.mu.Lock()
	if prior, ok := a.inbox.seenReceipts[cmd.ID]; ok && a.inbox.seenCommands[cmd.ID] == digest {
		a.mu.Unlock()
		return prior
	}
	for _, pending := range a.owner.rejections {
		if pending.receipt.CommandID == cmd.ID && pending.digest == digest {
			a.mu.Unlock()
			return pending.receipt
		}
	}
	select {
	case <-a.owner.done:
		a.mu.Unlock()
		return receipt
	default:
	}
	a.owner.rejections = append(a.owner.rejections, rejectedCommand{receipt, digest})
	a.mu.Unlock()
	a.ensureRun()
	select {
	case a.owner.control <- completionsReady{}:
	default:
	}
	return receipt
}
func (a *Agent) drainRejectedCommands() {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending := a.owner.rejections
	a.owner.rejections = nil
	for _, row := range pending {
		id := row.receipt.CommandID
		if prior, ok := a.inbox.seenCommands[id]; ok && prior != row.digest {
			continue
		}
		if _, ok := a.inbox.seenReceipts[id]; !ok {
			a.inbox.seenCommands[id] = row.digest
			a.inbox.seenReceipts[id] = row.receipt
		}
	}
}

// Cancellation signals have a fixed turn identity and fire at most once per
// command ID. Conditional commands wait for owner-side revision validation.
func (a *Agent) captureCommandCancellation(cmd protocol.Command) (*uint64, context.CancelFunc) {
	if cmd.Type != protocol.CommandInterrupt && cmd.Type != protocol.CommandStop {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq, ok := a.owner.cancellationSignals[cmd.ID]; ok {
		return &seq, nil
	}
	seq := a.sequences.turnSeq
	if a.owner.cancellationSignals == nil {
		a.owner.cancellationSignals = make(map[protocol.CommandID]uint64)
	}
	a.owner.cancellationSignals[cmd.ID] = seq
	if _, seen := a.inbox.seenCommands[cmd.ID]; seen || cmd.ExpectedRevision != 0 {
		return &seq, nil
	}
	return &seq, a.turn.turnCancel
}
