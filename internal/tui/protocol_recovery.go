package tui

import (
	"context"
	"strings"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

type operationRecovery struct {
	commandID protocol.CommandID
	operation protocol.OperationResult
	input     *protocol.InputResult
	err       error
}

type operationsRecoveredMsg struct {
	sessionID protocol.SessionID
	results   []operationRecovery
}

// The new Watch is already attached before this read starts, so a completion
// either appears in these durable results or arrives on the live subscription.
func (m Model) recoverOperationsCmd() tea.Cmd {
	reader, ok := m.client.(protocol.OperationReader)
	if !ok {
		return nil
	}
	var operations []Operation
	for _, op := range m.Operations() {
		_, needsReport := m.operationReports[op.CommandID]
		if op.Active() || needsReport {
			operations = append(operations, op)
		}
	}
	if len(operations) == 0 {
		return nil
	}
	sid := protocol.SessionID(m.sessionID)
	return func() tea.Msg {
		msg := operationsRecoveredMsg{sessionID: sid}
		for _, op := range operations {
			result := operationRecovery{commandID: op.CommandID}
			if op.Type == protocol.CommandSubmitInput {
				id := op.InputID
				if id == "" {
					id = protocol.InputID(op.CommandID)
				}
				input, err := reader.ReadInput(context.Background(), id)
				result.input, result.err = &input, err
			} else {
				result.operation, result.err = reader.ReadOperation(context.Background(), op.CommandID)
			}
			msg.results = append(msg.results, result)
		}
		return msg
	}
}

func (m *Model) applyRecoveredOperations(msg operationsRecoveredMsg) {
	for _, result := range msg.results {
		op, known := m.operation(result.commandID)
		_, report := m.operationReports[op.CommandID]
		if !known || !op.Active() && !report {
			// Command IDs are unique across attachments. A handle replacement
			// clears them; a live receipt or turn event may already settle them.
			continue
		}
		if result.err != nil {
			m.pushStatus("operation recovery: " + result.err.Error())
			continue
		}
		if input := result.input; input != nil {
			if input.Delivered {
				op.TurnID = input.TurnID
				m.operations[op.CommandID] = op
				m.consumeSubmission(op.CommandID)
			}
			switch input.Status {
			case protocol.TurnSucceeded, protocol.TurnFailed, protocol.TurnCancelled, protocol.TurnLimited, protocol.TurnBlocked:
				m.finishInputOperation(op, input.Status, input.Error)
			}
			continue
		}
		done := result.operation
		if !done.Complete {
			continue
		}
		receipt := protocol.Receipt{CommandID: op.CommandID, SessionID: msg.sessionID,
			OperationID: op.OperationID, Status: protocol.ReceiptApplied}
		if done.Status != "success" && done.Status != "applied" {
			code := done.Code
			if code == "" {
				code = protocol.ErrorInternal
			}
			receipt.Status = protocol.ReceiptRejected
			receipt.Error = &protocol.CommandError{Code: code, Message: done.Error}
		}
		m.applyReceipt(receipt, op.Purpose)
		if report && strings.TrimSpace(done.Output) != "" {
			kind := "system"
			if receipt.Rejected() {
				kind = "error"
			}
			m.addReport(historyCell{kind: kind, text: codingReport(done.Output), reportID: op.CommandID.String()})
		}
		delete(m.operationReports, op.CommandID)
	}
}
