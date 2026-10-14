package tui

import (
	"context"
	cryptorand "crypto/rand"

	"encoding/hex"

	"fmt"

	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/protocol"
)

var uiCommandSeq uint64

var uiProcessNonce = func() string {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}()

const maxReceiptKeys = 4096

func nextUICommandID() protocol.CommandID {
	return protocol.CommandID(fmt.Sprintf("tui-%s-%d", uiProcessNonce, atomic.AddUint64(&uiCommandSeq, 1)))
}

// submitCommand is the only mutation entry used by the TUI. A missing client
// is an explicit unavailable state; it never falls back to a second control
// channel or optimistically edits confirmed runtime state.
func (m *Model) submitCommand(cmd protocol.Command, purpose string) tea.Cmd {
	if m.watchDisconnected {
		m.pushStatus("connection unavailable; command not submitted · /reconnect")
		return nil
	}
	if cmd.ID == "" {
		cmd.ID = nextUICommandID()
	}
	if cmd.SessionID == "" {
		cmd.SessionID = protocol.SessionID(m.sessionID)
	}
	if cmd.ExpectedRevision == 0 && m.hasSnapshot && commandNeedsRevision(cmd.Type) {
		cmd.ExpectedRevision = m.snapshot.Revision.LogSeq
	}
	if cmd.ExpectedRunID == "" {
		cmd.ExpectedRunID = m.snapshot.RunID
	}
	if m.client == nil {
		m.pushStatus("session client unavailable")
		return nil
	}
	return m.submitPreparedCommand(cmd, purpose)
}

// Prepared commands carry their original envelope. Replays must not acquire
// the current run/revision or be rebuilt from a draft.
func (m *Model) submitPreparedCommand(cmd protocol.Command, purpose string) tea.Cmd {
	if m.watchDisconnected || m.client == nil {
		m.pushStatus("session connection unavailable; draft preserved")
		return nil
	}
	cmd = cloneInputCommand(cmd)
	m.registerOperation(cmd, purpose)
	m.beginActivity(cmd.ID, "", ActivitySubmitting, "正在提交", purpose)
	if commandProducesReport(cmd.Type) {
		if m.operationReports == nil {
			m.operationReports = make(map[protocol.CommandID]struct{})
		}
		m.operationReports[cmd.ID] = struct{}{}
	}
	// Receipts are asynchronous, so expose the submission boundary immediately
	// instead of leaving the previous notice in place while the runtime works.
	// The receipt/error path replaces this operation-scoped notice with its
	// terminal outcome.
	m.addNotice(Notice{CommandID: cmd.ID, Severity: NoticeInfo, Text: "submitting…", Sticky: false})
	client := m.client
	// A navigation cancellation owns observation only, never an accepted command.
	ctx := context.Background()
	sid := cmd.SessionID
	return func() tea.Msg {
		receipt, err := client.Submit(ctx, cmd)
		if err != nil {
			return commandErrorMsg{commandID: cmd.ID, sessionID: sid, err: err}
		}
		return commandReceiptMsg{receipt: receipt, sessionID: sid, purpose: purpose}
	}
}

func commandProducesReport(kind protocol.CommandType) bool {
	switch kind {
	case protocol.CommandExternal, protocol.CommandApply, protocol.CommandCheckpoint,
		protocol.CommandExport, protocol.CommandInit, protocol.CommandQuery,
		protocol.CommandRunWorkflow:
		return true
	default:
		return false
	}
}

func commandNeedsRevision(kind protocol.CommandType) bool {
	// Settings and environmental commands (workspace, reload, trust, memory, save,
	// workflows) are excluded: they merge against live authoritative state,
	// so pinning them to UI snapshot's LogSeq would cause spurious stale_revision
	// rejections during an active turn or after an asynchronous tool log increment.
	// Only explicit history-destructive CAS commands require revision locking.
	switch kind {
	case protocol.CommandClearConversation,
		protocol.CommandRemoveMessages,
		protocol.CommandRewindConversation,
		protocol.CommandFork:
		return true
	default:
		return false
	}
}

func (m *Model) applyReceipt(receipt protocol.Receipt, purpose string) {
	if receipt.SessionID != "" && receipt.SessionID.String() != m.sessionID {
		return
	}
	if receipt.CommandID != "" {
		key := receiptKey(receipt)
		if m.receiptKeys == nil {
			m.receiptKeys = make(map[protocol.CommandID]string)
		}
		if m.receiptKeys[receipt.CommandID] == key {
			return
		}
		if _, known := m.receiptKeys[receipt.CommandID]; !known {
			m.receiptOrder = append(m.receiptOrder, receipt.CommandID)
		}
		m.receiptKeys[receipt.CommandID] = key
		for len(m.receiptOrder) > maxReceiptKeys {
			oldest := m.receiptOrder[0]
			m.receiptOrder = m.receiptOrder[1:]
			delete(m.receiptKeys, oldest)
		}
	}
	op, knownOperation := m.operation(receipt.CommandID)
	if purpose == "" && knownOperation {
		purpose = op.Purpose
	}
	// Operation order is the ownership boundary for presentation. Once a newer
	// command has been accepted, a late receipt for an older command may still
	// close its own pending draft, but it cannot replace the current activity or
	// notice. This remains true after the newer command reaches a terminal state.
	currentOperation := knownOperation && m.operationOwnsPresentation(receipt.CommandID)
	// A command has one monotonic lifecycle. In particular, a delayed scheduled
	// receipt must not move an already-applied/rejected operation back to queued.
	if knownOperation && !op.Active() {
		return
	}
	if m.question != nil && m.question.pending == receipt.CommandID {
		if receipt.Rejected() {
			m.question.pending = ""
		} else {
			m.question = nil
		}
	}
	if receipt.Rejected() {
		if knownOperation {
			m.updateOperation(receipt.CommandID, OperationRejected, receipt)
		}
		delete(m.operationReports, receipt.CommandID)
		// A rejected receipt is terminal for this submission attempt. Keep the
		// draft in the explicit retry slot, but do not leave it in the
		// in-flight map: resume and session switching must be available after a
		// rejection, including when the user has started a new draft.
		m.restoreInputSubmission(receipt.CommandID, false)
		if m.approvalPending && receipt.CommandID == m.pendingApprovalCommand {
			m.approvalPending = false
			m.pendingApprovalCommand = ""
		}
		message := "command rejected"
		if receipt.Error != nil {
			if strings.Contains(receipt.Error.Message, "superseded") {
				// Cleanly superseded by a subsequent user action; do not pollute UI logs.
				return
			}
			message += ": " + receipt.Error.Error()
		}
		m.pushLog("error", message)
		if knownOperation && currentOperation {
			m.finishActivity(receipt.CommandID, OperationActivityPhase(receipt), message)
			m.clearNotice(receipt.CommandID)
			m.addNotice(Notice{CommandID: receipt.CommandID, Severity: NoticeError, Text: message, Sticky: true})
		}
		return
	}
	if receipt.Status == protocol.ReceiptScheduled {
		if knownOperation {
			m.updateOperation(receipt.CommandID, OperationQueued, receipt)
		}
		if knownOperation && currentOperation {
			if m.busy && op.Type != protocol.CommandSubmitInput {
				m.activity.Phase = ActivityRunningTool
				m.activity.Label = "正在执行"
				if m.snapshot.WorkLabel != "" {
					m.activity.Label = m.snapshot.WorkLabel
				}
			} else {
				m.activity.Phase = ActivityQueued
				m.activity.Label = "已排队"
			}
			m.activity.OperationID = receipt.OperationID
			m.activity.UpdatedAt = now()
			m.clearNotice(receipt.CommandID)
			m.addNotice(Notice{CommandID: receipt.CommandID, Severity: NoticeInfo, Text: "queued", Sticky: false})
		}
		return
	}
	if knownOperation {
		m.updateOperation(receipt.CommandID, OperationApplied, receipt)
	}
	m.consumeSubmission(receipt.CommandID)
	if m.inputRetry != nil && receipt.CommandID == m.inputRetry.command.ID {
		m.inputRetry = nil
	}
	if m.approvalPending && receipt.CommandID == m.pendingApprovalCommand {
		if m.pendingApprovalKey == m.approvalKey() {
			m.approval = nil
			m.clearDecisionNotices()
		}
		m.approvalPending = false
		m.pendingApprovalCommand = ""
		m.pendingApprovalKey = ""
	}
	if knownOperation && currentOperation {
		m.finishActivity(receipt.CommandID, ActivityCompleted, purpose)
		m.clearNotice(receipt.CommandID)
		if purpose != "" {
			m.addNotice(Notice{CommandID: receipt.CommandID, Severity: NoticeSuccess, Text: purpose, Sticky: false})
		}
	}
}

func (m Model) operationOwnsPresentation(commandID protocol.CommandID) bool {
	target, ok := m.operations[commandID]
	if !ok {
		return false
	}
	for id, op := range m.operations {
		if id == commandID {
			continue
		}
		if op.Order > target.Order {
			return false
		}
	}
	return true
}

func OperationActivityPhase(receipt protocol.Receipt) ActivityPhase {
	if receipt.Error != nil && receipt.Error.Code == protocol.ErrorClosed {
		return ActivityCancelled
	}
	return ActivityFailed
}

func receiptKey(receipt protocol.Receipt) string {
	errorText := ""
	if receipt.Error != nil {
		errorText = receipt.Error.Error()
	}
	return fmt.Sprintf("%s|%d|%d|%s", receipt.Status, receipt.Revision.LogSeq, receipt.Revision.ViewGeneration, errorText)
}

// A transport failure does not establish whether the input was accepted. Keep
// its original command for an explicit, idempotent retry. Confirmed refusals
// use restoreInputSubmission with replay=false instead.
func (m *Model) restoreFailedSubmission(commandID protocol.CommandID) {
	m.restoreInputSubmission(commandID, true)
}

func cloneInputCommand(cmd protocol.Command) protocol.Command {
	if cmd.Input != nil {
		input := *cmd.Input
		input.Images = cloneInputImages(input.Images)
		cmd.Input = &input
	}
	return cmd
}

func (m *Model) restoreInputSubmission(commandID protocol.CommandID, replay bool) {
	op, ok := m.operations[commandID]
	if !ok || !op.Recoverable || op.InputCommand == nil {
		return
	}
	command := cloneInputCommand(*op.InputCommand)
	m.consumeSubmission(commandID)
	m.inputRetry = &inputRetry{command: command, replay: replay}
	text, images := command.Input.Text, command.Input.Images
	if strings.TrimSpace(m.textarea.Value()) == "" && len(m.inputImages) == 0 && m.pasteRequest == "" {
		m.missingHistoryImages = false
		m.inputImages = cloneInputImages(images)
		m.selectedImage = max(0, len(images)-1)
		m.textarea.SetValue(text)
		m.textarea.CursorEnd()
		m.syncInputHeight()
		m.refreshCmdSuggest()
		m.layout()
		return
	}
	m.pushStatus("message submission failed; draft is available for retry")
}

func (m *Model) forgetReceipt(id protocol.CommandID) {
	delete(m.receiptKeys, id)
	for i, known := range m.receiptOrder {
		if known == id {
			m.receiptOrder = append(m.receiptOrder[:i], m.receiptOrder[i+1:]...)
			break
		}
	}
}
