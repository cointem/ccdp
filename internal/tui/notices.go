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

// composerFeedback reports whether a notice answers a submitted command rather
// than the conversation: a plain acknowledgement ("effort → xhigh", "history
// cleared") carrying the id of the command that produced it. Receipts belong to
// the input's hint lane, because next to the prompt they cannot be mistaken for
// one more row of output. Decisions, errors, warnings and free-standing status
// messages stay in the status lane.
func (n Notice) composerFeedback() bool {
	if n.CommandID == "" || n.Sticky {
		return false
	}
	return n.Severity == NoticeInfo || n.Severity == NoticeSuccess
}

type noticeTickMsg struct{ at time.Time }

const (
	shortNoticeDuration = 4 * time.Second
	noticeTickInterval  = 250 * time.Millisecond
)

func noticeTickCmd() tea.Cmd {
	return tea.Tick(noticeTickInterval, func(at time.Time) tea.Msg { return noticeTickMsg{at: at} })
}

type noticeStore struct{ notices []Notice }

func (m *Model) addNotice(notice Notice) {
	if notice.SessionID == "" {
		notice.SessionID = protocol.SessionID(m.sessionID)
	}
	m.noticeStore.add(notice)
}

func (m *noticeStore) add(notice Notice) {
	if strings.TrimSpace(notice.Text) == "" {
		return
	}
	if notice.ID == "" {
		notice.ID = nextUICommandID().String()
	}
	if notice.CreatedAt.IsZero() {
		notice.CreatedAt = now()
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
				return
			}
		}
	}
	m.notices = append(m.notices, notice)
	if len(m.notices) > 32 {
		m.notices = append([]Notice(nil), m.notices[len(m.notices)-32:]...)
	}
}

func (m *noticeStore) latestNotice() (Notice, bool) {
	nowAt := now()
	for i := len(m.notices) - 1; i >= 0; i-- {
		if !m.notices[i].Expired(nowAt) {
			return m.notices[i], true
		}
	}
	return Notice{}, false
}

// latestStatusNotice returns the newest live notice that belongs to the status
// lane. Command receipts are excluded because the composer renders them instead.
func (m *noticeStore) latestStatusNotice() (Notice, bool) {
	nowAt := now()
	for i := len(m.notices) - 1; i >= 0; i-- {
		notice := m.notices[i]
		if notice.Expired(nowAt) || notice.composerFeedback() {
			continue
		}
		return notice, true
	}
	return Notice{}, false
}

// latestFeedback returns the newest live command receipt for the composer's
// hint lane.
func (m *noticeStore) latestFeedback() (Notice, bool) {
	nowAt := now()
	for i := len(m.notices) - 1; i >= 0; i-- {
		notice := m.notices[i]
		if notice.Expired(nowAt) || !notice.composerFeedback() {
			continue
		}
		return notice, true
	}
	return Notice{}, false
}

func (m *noticeStore) expireNotices(at time.Time) bool {
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
	return changed
}

func (m *noticeStore) clearNotice(commandID protocol.CommandID) {
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
}

func (m *noticeStore) clearTransientNotices() {
	kept := m.notices[:0]
	for _, notice := range m.notices {
		if !notice.Sticky {
			continue
		}
		kept = append(kept, notice)
	}
	m.notices = kept
}

func (m *noticeStore) clearDecisionNotices() {
	kept := m.notices[:0]
	for _, notice := range m.notices {
		if notice.Severity != NoticeDecision {
			kept = append(kept, notice)
		}
	}
	m.notices = kept
}

func (m Model) visibleNotices() []Notice { return m.noticeStore.visible(m.sessionID) }

func (m noticeStore) visible(sessionID string) []Notice {
	at := now()
	out := make([]Notice, 0, len(m.notices))
	for _, notice := range m.notices {
		if notice.SessionID != "" && sessionID != "" && notice.SessionID.String() != sessionID {
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
	InputImages      []protocol.InputImage
	Recoverable      bool
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

type operationStore struct {
	operations      map[protocol.CommandID]Operation
	operationSeq    uint64
	latestOperation protocol.CommandID
}

func (m *Model) registerOperation(cmd protocol.Command, purpose string) {
	if cmd.Type == protocol.CommandSubmitInput || cmd.Input != nil {
		m.retireTerminalErrorNotices(cmd.ID)
	}
	m.operationStore.register(cmd, purpose)
}

func (m *operationStore) register(cmd protocol.Command, purpose string) {
	if cmd.ID == "" {
		return
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
		op.InputImages = cloneInputImages(cmd.Input.Images)
		op.Recoverable = true
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
	}
}

// pruneOperations bounds UI bookkeeping without evicting a command that can
// still receive a receipt. Active operations and the latest terminal owner
// remain available for stale-receipt ownership checks; old terminal records
// carry no additional authority once their receipt has been observed.
func (m *operationStore) pruneOperations() {
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

func (m *operationStore) operation(commandID protocol.CommandID) (Operation, bool) {
	op, ok := m.operations[commandID]
	return op, ok
}

func (m *operationStore) updateOperation(commandID protocol.CommandID, state OperationState, receipt protocol.Receipt) (Operation, bool) {
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

func (m Model) PendingInputs() []protocol.InputView {
	return append([]protocol.InputView(nil), m.pendingInputs...)
}

func (m Model) Activity() Activity { return m.activity }

func (m Model) Notices() []Notice { return m.visibleNotices() }

func (m operationStore) Operations() []Operation {
	out := make([]Operation, 0, len(m.operations))
	for _, op := range m.operations {
		op.InputImages = cloneInputImages(op.InputImages)
		out = append(out, op)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

func (m operationStore) pendingSubmissionCount() int {
	n := 0
	for _, op := range m.operations {
		if op.Recoverable {
			n++
		}
	}
	return n
}

func (m *operationStore) consumeSubmission(id protocol.CommandID) {
	if op, ok := m.operations[id]; ok {
		op.Recoverable = false
		op.InputImages = nil
		m.operations[id] = op
	}
}
