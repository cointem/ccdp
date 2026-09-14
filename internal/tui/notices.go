package tui

import (
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/protocol"
)

// NoticeSeverity controls ordering and visual emphasis in the presentation
// layer. The text remains meaningful without color.
type NoticeSeverity string

const (
	NoticeInfo     NoticeSeverity = "info"
	NoticeSuccess  NoticeSeverity = "success"
	NoticeWarning  NoticeSeverity = "warning"
	NoticeError    NoticeSeverity = "error"
	NoticeDecision NoticeSeverity = "decision"
)

// Notice is a short lived, operation-scoped feedback item. Decision and error
// notices are sticky until the owning state changes; ordinary acknowledgements
// expire after shortNoticeDuration.
type Notice struct {
	ID        string
	SessionID protocol.SessionID
	CommandID protocol.CommandID
	Severity  NoticeSeverity
	Text      string
	CreatedAt time.Time
	ExpiresAt time.Time
	Sticky    bool
}

func (n Notice) Expired(at time.Time) bool {
	return !n.Sticky && !n.ExpiresAt.IsZero() && !at.Before(n.ExpiresAt)
}

type noticeTickMsg struct{ at time.Time }

const (
	shortNoticeDuration = 4 * time.Second
	noticeTickInterval  = 250 * time.Millisecond
)

func noticeTickCmd() tea.Cmd {
	return tea.Tick(noticeTickInterval, func(at time.Time) tea.Msg { return noticeTickMsg{at: at} })
}

func (m *Model) addNotice(notice Notice) {
	if strings.TrimSpace(notice.Text) == "" {
		return
	}
	if notice.ID == "" {
		notice.ID = nextUICommandID().String()
	}
	if notice.CreatedAt.IsZero() {
		notice.CreatedAt = now()
	}
	if notice.SessionID == "" {
		notice.SessionID = protocol.SessionID(m.sessionID)
	}
	if notice.Severity == "" {
		notice.Severity = NoticeInfo
	}
	if notice.ExpiresAt.IsZero() && !notice.Sticky {
		notice.ExpiresAt = notice.CreatedAt.Add(shortNoticeDuration)
	}
	// The same operation can emit a preview and a receipt. Replace an existing
	// notice for that owner so the user sees one coherent lifecycle.
	if notice.CommandID != "" {
		for i := range m.notices {
			if m.notices[i].CommandID == notice.CommandID {
				m.notices[i] = notice
				m.syncLegacyStatus()
				return
			}
		}
	}
	m.notices = append(m.notices, notice)
	if len(m.notices) > 32 {
		m.notices = append([]Notice(nil), m.notices[len(m.notices)-32:]...)
	}
	m.syncLegacyStatus()
}

func (m *Model) noticeForCommand(commandID protocol.CommandID) (Notice, bool) {
	for i := len(m.notices) - 1; i >= 0; i-- {
		if m.notices[i].CommandID == commandID {
			return m.notices[i], true
		}
	}
	return Notice{}, false
}

func (m *Model) latestNotice() (Notice, bool) {
	nowAt := now()
	for i := len(m.notices) - 1; i >= 0; i-- {
		if !m.notices[i].Expired(nowAt) {
			return m.notices[i], true
		}
	}
	return Notice{}, false
}

func (m *Model) expireNotices(at time.Time) bool {
	if len(m.notices) == 0 {
		return false
	}
	kept := m.notices[:0]
	changed := false
	for _, notice := range m.notices {
		if notice.Expired(at) {
			changed = true
			continue
		}
		kept = append(kept, notice)
	}
	m.notices = kept
	if changed {
		m.syncLegacyStatus()
	}
	return changed
}

// syncLegacyStatus keeps old in-package callers working while all new
// presentation reads notices. It only changes the compatibility field when a
// notice owns it, so a runtime test that sets status directly remains intact.
func (m *Model) syncLegacyStatus() {
	if notice, ok := m.latestNotice(); ok {
		m.status = notice.Text
		return
	}
	if m.status != "" {
		m.status = ""
	}
}

func (m *Model) clearNotice(commandID protocol.CommandID) {
	if commandID == "" {
		return
	}
	kept := m.notices[:0]
	for _, notice := range m.notices {
		if notice.CommandID != commandID {
			kept = append(kept, notice)
		}
	}
	m.notices = kept
	m.syncLegacyStatus()
}

func (m *Model) clearTransientNotices() {
	kept := m.notices[:0]
	for _, notice := range m.notices {
		if !notice.Sticky {
			continue
		}
		kept = append(kept, notice)
	}
	m.notices = kept
	m.syncLegacyStatus()
}

func (m Model) visibleNotices() []Notice {
	at := now()
	out := make([]Notice, 0, len(m.notices))
	for _, notice := range m.notices {
		if notice.SessionID != "" && m.sessionID != "" && notice.SessionID.String() != m.sessionID {
			continue
		}
		if !notice.Expired(at) {
			out = append(out, notice)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return noticePriority(out[i].Severity) > noticePriority(out[j].Severity)
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func noticePriority(severity NoticeSeverity) int {
	switch severity {
	case NoticeDecision:
		return 4
	case NoticeError:
		return 3
	case NoticeWarning:
		return 2
	case NoticeSuccess:
		return 1
	default:
		return 0
	}
}

// OperationState follows a command from submission through the runtime
// receipt. A scheduled operation stays pending until its terminal receipt.
type OperationState string

const (
	OperationPending   OperationState = "pending"
	OperationQueued    OperationState = "queued"
	OperationApplied   OperationState = "applied"
	OperationRejected  OperationState = "rejected"
	OperationFailed    OperationState = "failed"
	OperationCancelled OperationState = "cancelled"
)

const maxOperationHistory = 256

// Operation is a renderer-facing record of one protocol command. It preserves
// the expected revision/run identity so a late receipt can never overwrite the
// current operation's activity or confirmed settings.
type Operation struct {
	CommandID   protocol.CommandID
	OperationID protocol.OperationID
	SessionID   protocol.SessionID
	RunID       protocol.RunID
	// InputID/InputText/TurnID link a submit_input admission to the turn
	// lifecycle. Runtime admission returns a scheduled receipt for inputs and
	// intentionally has no second applied receipt, so the TUI closes that
	// operation when the associated turn reaches its terminal boundary.
	InputID          protocol.InputID
	InputText        string
	TurnID           protocol.TurnID
	Type             protocol.CommandType
	Purpose          string
	State            OperationState
	ExpectedRevision uint64
	ExpectedRunID    protocol.RunID
	StartedAt        time.Time
	UpdatedAt        time.Time
	FinishedAt       time.Time
	Error            string
	Order            uint64
}

func (o Operation) Active() bool {
	return o.State == OperationPending || o.State == OperationQueued
}

func (m *Model) registerOperation(cmd protocol.Command, purpose string) {
	if cmd.ID == "" {
		return
	}
	// A fresh user turn supersedes the visibility of a previous command's
	// terminal error. The durable error report remains in the transcript, but
	// the status lane must describe the operation the user is acting on now.
	// Command-less notices are connection/runtime state and intentionally stay.
	if cmd.Type == protocol.CommandSubmitInput || cmd.Input != nil {
		m.retireTerminalErrorNotices(cmd.ID)
	}
	if m.operations == nil {
		m.operations = make(map[protocol.CommandID]Operation)
	}
	when := now()
	m.operationSeq++
	op := Operation{CommandID: cmd.ID, SessionID: cmd.SessionID, RunID: cmd.ExpectedRunID,
		ExpectedRunID: cmd.ExpectedRunID, ExpectedRevision: cmd.ExpectedRevision, Type: cmd.Type,
		Purpose: purpose, State: OperationPending, StartedAt: when, UpdatedAt: when, Order: m.operationSeq}
	if cmd.Input != nil {
		op.InputID, op.InputText = cmd.Input.ID, cmd.Input.Text
	}
	m.operations[cmd.ID] = op
	m.latestOperation = cmd.ID
	m.pruneOperations()
}

// retireTerminalErrorNotices removes command-scoped errors that have already
// reached a terminal operation state when a new user operation starts. A
// failed command is still represented by its durable transcript report, while
// an old sticky toast must not mask the new operation's activity.
func (m *Model) retireTerminalErrorNotices(current protocol.CommandID) {
	if len(m.notices) == 0 {
		return
	}
	kept := m.notices[:0]
	changed := false
	for _, notice := range m.notices {
		if notice.Severity != NoticeError || notice.CommandID == "" || notice.CommandID == current {
			kept = append(kept, notice)
			continue
		}
		op, known := m.operations[notice.CommandID]
		if known && op.Active() {
			kept = append(kept, notice)
			continue
		}
		changed = true
	}
	if changed {
		m.notices = kept
		m.syncLegacyStatus()
	}
}

// pruneOperations bounds UI bookkeeping without evicting a command that can
// still receive a receipt. Active operations and the latest terminal owner
// remain available for stale-receipt ownership checks; old terminal records
// carry no additional authority once their receipt has been observed.
func (m *Model) pruneOperations() {
	if len(m.operations) <= maxOperationHistory {
		return
	}
	protected := make(map[protocol.CommandID]struct{})
	for id, op := range m.operations {
		if op.Active() || id == m.latestOperation {
			protected[id] = struct{}{}
		}
	}
	type candidate struct {
		id    protocol.CommandID
		order uint64
	}
	candidates := make([]candidate, 0, len(m.operations))
	for id, op := range m.operations {
		if _, keep := protected[id]; keep || op.Active() {
			continue
		}
		candidates = append(candidates, candidate{id: id, order: op.Order})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].order < candidates[j].order })
	for _, item := range candidates {
		if len(m.operations) <= maxOperationHistory {
			break
		}
		delete(m.operations, item.id)
	}
}

func (m *Model) operation(commandID protocol.CommandID) (Operation, bool) {
	op, ok := m.operations[commandID]
	return op, ok
}

func (m *Model) updateOperation(commandID protocol.CommandID, state OperationState, receipt protocol.Receipt) (Operation, bool) {
	op, ok := m.operations[commandID]
	if !ok {
		return Operation{}, false
	}
	if receipt.OperationID != "" {
		op.OperationID = receipt.OperationID
	}
	op.State = state
	op.UpdatedAt = now()
	if state == OperationApplied || state == OperationRejected || state == OperationFailed || state == OperationCancelled {
		op.FinishedAt = op.UpdatedAt
	}
	if receipt.Error != nil {
		op.Error = receipt.Error.Error()
	}
	m.operations[commandID] = op
	return op, true
}

func (m Model) pendingOperations() []Operation {
	return m.activeOperations()
}

func (m Model) PendingInputs() []protocol.InputView {
	return append([]protocol.InputView(nil), m.pendingInputs...)
}

func (m Model) Activity() Activity { return m.activity }

func (m Model) Notices() []Notice { return m.visibleNotices() }

func (m Model) Operations() []Operation {
	out := make([]Operation, 0, len(m.operations))
	for _, op := range m.operations {
		out = append(out, op)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}
