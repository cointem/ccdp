package tui

import (
	"ccdp/internal/protocol"
)

// associateInputOperation links a scheduled submit_input command to the turn
// announced by the runtime. Input commands intentionally receive an admission
// receipt (scheduled) rather than a second terminal receipt; this turn identity
// is the lifecycle boundary used to retire their UI operation.
func (m *Model) associateInputOperationFromEvent(ev protocol.EventView) {
	if ev.Kind != protocol.EventUserMessage || ev.SessionID != "" && ev.SessionID.String() != m.sessionID {
		return
	}
	for _, candidate := range m.activeInputOperations() {
		if candidate.TurnID != "" {
			continue
		}
		if candidate.InputID == "" || protocol.InputMessageID(candidate.InputID) != ev.MessageID {
			continue
		}
		candidate.TurnID = ev.TurnID
		candidate.UpdatedAt = now()
		m.operations[candidate.CommandID] = candidate
		if m.activity.CommandID == candidate.CommandID {
			m.activity.TurnID = ev.TurnID
			m.activity.UpdatedAt = candidate.UpdatedAt
		}
		return
	}
}

func (m *Model) associateInputOperationsFromSnapshot(snapshot protocol.SessionView) {
	if len(snapshot.Transcript) == 0 || len(m.operations) == 0 {
		return
	}
	for _, item := range snapshot.Transcript {
		if item.Kind != "user" || item.TurnID == "" {
			continue
		}
		for _, candidate := range m.activeInputOperations() {
			if candidate.TurnID != "" || candidate.InputID == "" || protocol.InputMessageID(candidate.InputID) != item.ID {
				continue
			}
			candidate.TurnID = item.TurnID
			candidate.UpdatedAt = now()
			m.operations[candidate.CommandID] = candidate
			if m.activity.CommandID == candidate.CommandID {
				m.activity.TurnID = item.TurnID
				m.activity.UpdatedAt = candidate.UpdatedAt
			}
			break
		}
	}
}

func (m Model) activeInputOperations() []Operation {
	active := m.activeOperations()
	out := active[:0]
	for _, op := range active {
		if op.Type == protocol.CommandSubmitInput {
			out = append(out, op)
		}
	}
	return out
}

// finishInputOperations retires submit_input operations at the runtime's
// terminal turn boundary. A late/older operation can close its own draft, but
// only the newest operation may replace the visible activity or notice.
func (m *Model) finishInputOperations(turnID protocol.TurnID, outcome protocol.TurnOutcomeStatus, detail string) {
	if len(m.operations) == 0 {
		return
	}
	for _, op := range m.activeInputOperations() {
		if turnID != "" && canonicalUITurn(op.TurnID) != canonicalUITurn(turnID) {
			continue
		}
		if turnID == "" && op.TurnID != "" {
			continue
		}
		m.finishInputOperation(op, outcome, detail)
	}
}

func (m *Model) finishInputOperation(op Operation, outcome protocol.TurnOutcomeStatus, detail string) {
	state, phase := OperationApplied, ActivityCompleted
	switch outcome {
	case protocol.TurnFailed, protocol.TurnLimited, protocol.TurnBlocked:
		state, phase = OperationFailed, ActivityFailed
	case protocol.TurnCancelled:
		state, phase = OperationCancelled, ActivityCancelled
	}
	var receipt protocol.Receipt
	receipt.CommandID, receipt.SessionID = op.CommandID, op.SessionID
	receipt.Status = protocol.ReceiptApplied
	if state == OperationFailed || state == OperationCancelled {
		receipt.Status = protocol.ReceiptRejected
		code := protocol.ErrorInternal
		if state == OperationCancelled {
			code = protocol.ErrorClosed
		}
		receipt.Error = &protocol.CommandError{Code: code, Message: detail}
	}
	m.updateOperation(op.CommandID, state, receipt)
	// A turn outcome proves the runtime already consumed this input.
	// Provider failure or cancellation must not put a sent message back in
	// the composer (or reuse its command ID as an unaccepted retry). Only
	// submission rejection/transport failure restores an unsent draft.
	m.consumeSubmission(op.CommandID)
	if m.inputRetry != nil && op.CommandID == m.inputRetry.command.ID {
		m.inputRetry = nil
	}
	if m.operationOwnsPresentation(op.CommandID) {
		if state == OperationApplied {
			m.finishActivity(op.CommandID, phase, "")
			m.clearNotice(op.CommandID)
		} else {
			message := "message turn failed"
			if state == OperationCancelled {
				message = "message turn cancelled"
			}
			if detail != "" {
				message += ": " + detail
			}
			m.finishActivity(op.CommandID, phase, detail)
			m.clearNotice(op.CommandID)
			if state == OperationCancelled || !m.hasTurnError(op.TurnID, detail) {
				m.addNotice(Notice{CommandID: op.CommandID, Severity: NoticeError, Text: message, Sticky: true})
			}
		}
	}
}
