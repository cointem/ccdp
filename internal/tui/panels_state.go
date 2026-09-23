package tui

import (
	"ccdp/internal/protocol"
)

// panelState owns the state of its component within a session.
type panelState struct {
	manager         PanelManager
	question        *questionState
	approval        *approvalPrompt
	approvalPending bool
	// mouseReleasePending is set when a selector closes immediately before a
	// protocol command. The receipt/error path restores transcript mouse reporting after the
	// command result so direct command callers still receive the typed receipt.
	mouseReleasePending bool
	// pendingApprovalCommand identifies the one approval decision currently
	// awaiting a receipt. A receipt for an unrelated command must never close
	// the modal while commands complete out of order on the watch.
	pendingApprovalCommand protocol.CommandID
	// approvalState owns scrolling for long commands/plans.
	approvalState ScrollState
	// approvalCursor is the ↑↓-selected choice inside the approval modal
	// (0=allow once, 1=always allow, 2=deny; Plan uses 0=approve, 1=deny).
	approvalCursor     int
	approvalSelection  string
	pendingApprovalKey string
	approvalSelected   bool
	deferredApproval   string
	exitConfirm        bool
	// picker drives scrollable list selection (e.g. /rewind and /resume).
	picker *selectorPanel

	modalErr string // transient message inside the modal area

}

// selectorPanel owns one selection model and the action attached to it.
type selectorPanel struct {
	Selector
	action       selectorAction
	effortMotion effortMotion
	buf          string
	inline       bool
}

// approvalPrompt is the UI projection of a protocol decision, never a runtime handle.
type approvalPrompt struct {
	ID, Tool, Command, Reason string
}
