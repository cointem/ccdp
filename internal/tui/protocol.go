package tui

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/permissions"
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

// applySnapshot is the only path that confirms runtime-owned settings in the
// TUI. Events are previews; a state snapshot is authoritative and is copied
// before it is retained by the Model.
func (m *Model) applySnapshot(snapshot protocol.SessionView) {
	if m.hasSnapshot && snapshot.SessionID.String() == m.sessionID && snapshot.RunID == m.snapshot.RunID &&
		snapshotRevisionOlder(snapshot.Revision, m.snapshot.Revision) {
		// A delayed resync must not roll settings, activity, or pending inputs
		// back over a newer snapshot. Equal revisions are still accepted because
		// runtime phase and decision state may change without a log append.
		return
	}
	// Queue/settings refreshes are not evidence of model output. Preserve
	// the last progress time when the runtime phase and transcript are unchanged.
	sameProgress := m.hasSnapshot && snapshot.SessionID == m.snapshot.SessionID &&
		snapshot.RunID == m.snapshot.RunID && snapshot.Busy == m.snapshot.Busy &&
		snapshot.Phase == m.snapshot.Phase && reflect.DeepEqual(snapshot.Transcript, m.snapshot.Transcript)
	previousActivity := m.activity
	previousSession := m.sessionID
	sessionChanged := previousSession != "" && previousSession != snapshot.SessionID.String()
	m.snapshot = snapshot
	m.hasSnapshot = true
	m.sessionID = snapshot.SessionID.String()
	if sessionChanged {
		m.reportGeneration++
		// A session swap starts a new terminal transcript identity space. The
		// first snapshot of the new session remains visible in the managed
		// frame and is then used as the new inline baseline.
		m.inline.forgetAll()
		m.operationReports = nil
		m.receiptKeys = nil
		m.receiptOrder = nil
	}
	m.busy = snapshot.Busy
	m.pendingInputs = clonePendingInputs(snapshot.PendingInputs)
	m.modelName = snapshot.Settings.Model.Model
	if snapshot.Settings.Permission.Mode != "" {
		m.mode = permissions.Mode(snapshot.Settings.Permission.Mode)
	}
	m.planMode = snapshot.Settings.ExecutionMode == protocol.ExecutionModePlan
	incomingActivity := m.activityForSnapshot(snapshot)
	if sameProgress && incomingActivity.Phase == previousActivity.Phase && !previousActivity.UpdatedAt.IsZero() {
		incomingActivity.UpdatedAt = previousActivity.UpdatedAt
		incomingActivity.StartedAt = previousActivity.StartedAt
	}
	if current, ok := m.operation(m.latestOperation); ok && current.Active() &&
		(incomingActivity.Phase == ActivityIdle || incomingActivity.Phase == "") {
		// An idle snapshot can race the command receipt. Keep the submission
		// activity until the owning receipt closes it; this prevents a stale
		// state update from claiming the operation completed.
		m.activity.Detail = current.Purpose
	} else {
		m.setActivity(incomingActivity)
	}
	m.usage = protocol.UsageSnapshot{
		InputTokens: snapshot.Usage.InputTokens, OutputTokens: snapshot.Usage.OutputTokens,
		CachedTokens: snapshot.Usage.CachedTokens, Cache: snapshot.Usage.Cache, Cost: snapshot.Usage.Cost,
		TurnCount: snapshot.Usage.TurnCount,
	}
	// A conversation reset (/clear, a rewind, a replaced history projection)
	// reaches the UI as a same-session snapshot with no transcript while rows of
	// the discarded conversation are still on screen. Only the runtime decides
	// that; the UI merely re-baselines, since native scrollback rows cannot be
	// un-printed by a managed-frame repaint. A session swap is excluded because
	// it owns the presentation transition itself.
	if !sessionChanged && len(m.confirmedItems) > 0 && len(snapshot.Transcript) == 0 {
		m.resetNativeHistory()
	}
	m.applyTranscript(snapshot.Transcript)
	m.associateInputOperationsFromSnapshot(snapshot)
	if snapshot.LastTurn != nil && snapshot.Phase == protocol.PhaseIdle && !snapshot.Busy {
		m.finishInputOperations(snapshot.LastTurn.TurnID, snapshot.LastTurn.Status, snapshot.LastTurn.Error)
	}
	if sessionChanged {
		m.inline.prime(m.items)
	}
	m.turnDone = !snapshot.Busy
	if m.turnDone && m.interruptRequested {
		m.turnFailed = true
	}
	m.setQuestion(snapshot.Question)
	if snapshot.Approval != nil {
		m.approval = approvalRequestFromView(snapshot.Approval)
	} else if snapshot.Plan != nil {
		m.approval = &approvalPrompt{ID: snapshot.Plan.ID, Tool: "Plan", Command: snapshot.Plan.Text, Reason: "Approve this plan and start executing?"}
	} else {
		m.approval = nil
		m.clearDecisionNotices()
	}
	if snapshot.Revision.LogSeq > m.watchCursor.LogSeq {
		m.watchCursor.LogSeq = snapshot.Revision.LogSeq
	}
	m.watchCursor.ViewGeneration = snapshot.Revision.ViewGeneration
}

func (m *Model) composeItems() {
	m.CellStore.compose()
	m.attachAgentMarkers()
}

func (m *Model) appendTranscript(item historyCell) {
	if item.messageID == "" && item.reportID == "" {
		item.reportID = nextUICommandID().String()
	}
	m.confirmedItems = append(m.confirmedItems, item)
	m.composeItems()
}

// Live events use numeric turn IDs; the durable journal prefixes them.
func canonicalUITurn(id protocol.TurnID) string {
	s := id.String()
	if _, err := strconv.ParseUint(s, 10, 64); err == nil {
		return "turn-" + s
	}
	return s
}

func (m *Model) hasTurnError(turnID protocol.TurnID, text string) bool {
	if turnID == "" {
		return false
	}
	for _, item := range m.items {
		if item.kind == "error" && (text == "" || item.text == text) && canonicalUITurn(item.turnID) == canonicalUITurn(turnID) {
			return true
		}
	}
	return false
}

// Replace the live error report with its durable row without printing it a
// second time. Matching both turn and source preserves equal errors in other
// turns and distinct diagnostics from the same turn.
func (m *Model) reconcileErrorReport(item historyCell) {
	if item.kind != "error" || item.turnID == "" {
		return
	}
	kept := m.reports[:0]
	for _, report := range m.reports {
		if report.kind != "error" || report.text != item.text || canonicalUITurn(report.turnID) != canonicalUITurn(item.turnID) {
			kept = append(kept, report)
			continue
		}
		m.inline.ensure()
		oldID, newID := itemIdentity(report, 0), itemIdentity(item, 0)
		if _, printed := m.inline.printed[oldID]; printed {
			m.inline.printed[newID] = struct{}{}
		}
		if offset, exists := m.inline.offsets[inlineLogicalKey(report, 0)]; exists {
			m.inline.offsets[inlineLogicalKey(item, 0)] = offset
		}
	}
	m.reports = kept
}

func (m *Model) addReport(item historyCell) {
	if item.reportID == "" {
		item.reportID = nextUICommandID().String()
	} else {
		// Runtime watch resync can replay an operation completion.  Reports are
		// durable presentation units, so a known identity is emitted exactly
		// once even when the event is observed twice.
		for _, existing := range m.reports {
			if existing.reportID == item.reportID {
				return
			}
		}
	}
	item.reportIndex = len(m.confirmedItems)
	if len(m.confirmedItems) > 0 {
		item.reportAnchor = m.confirmedItems[len(m.confirmedItems)-1].messageID
	}
	m.reports = append(m.reports, item)
	if m.routing != nil && len(m.reports) > 128 {
		m.reports = append([]historyCell(nil), m.reports[len(m.reports)-128:]...)
	}
	m.composeItems()
}

// querySessionClient is intentionally optional: the public SessionClient
// contract stays small for remote/recording adapters, while Agent exposes a
// direct bounded Query implementation.  When this extension is absent,
// runQuery submits the same typed CommandQuery through the normal protocol.
type querySessionClient interface {
	Query(context.Context, protocol.QueryKind) (protocol.QueryReport, error)
}

func (m *Model) runQuery(kind protocol.QueryKind, purpose string) tea.Cmd {
	if m.client == nil {
		m.pushStatus("session client unavailable")
		return nil
	}
	client := m.client
	ctx := m.watchCtx
	if ctx == nil {
		ctx = context.Background()
	}
	sid := protocol.SessionID(m.sessionID)
	commandID := nextUICommandID()
	cmd := protocol.Command{ID: commandID, SessionID: sid, Type: protocol.CommandQuery,
		Query: &protocol.QueryCommand{Kind: kind}}
	if queryClient, ok := client.(querySessionClient); ok {
		m.registerOperation(cmd, purpose)
		m.beginActivity(commandID, "", ActivitySubmitting, "正在提交", purpose)
		m.addNotice(Notice{CommandID: commandID, Severity: NoticeInfo, Text: "submitting…", Sticky: false})
		return func() tea.Msg {
			report, err := queryClient.Query(ctx, kind)
			return queryReportMsg{commandID: commandID, report: report, sessionID: sid, err: err}
		}
	}
	return m.submitCommand(cmd, purpose)
}

func (m *Model) applyQueryReport(msg queryReportMsg) {
	if msg.sessionID != "" && msg.sessionID.String() != m.sessionID {
		return
	}
	if op, ok := m.operation(msg.commandID); ok {
		if msg.err != nil {
			m.updateOperation(msg.commandID, OperationFailed, protocol.Receipt{CommandID: msg.commandID, SessionID: msg.sessionID, Error: &protocol.CommandError{Message: msg.err.Error()}})
			m.finishActivity(msg.commandID, ActivityFailed, msg.err.Error())
		} else {
			m.updateOperation(msg.commandID, OperationApplied, protocol.Receipt{CommandID: msg.commandID, SessionID: msg.sessionID})
			m.finishActivity(msg.commandID, ActivityCompleted, op.Purpose)
		}
		m.clearNotice(msg.commandID)
	}
	if msg.err != nil {
		m.addReport(historyCell{kind: "error", text: "query failed: " + msg.err.Error(), reportID: msg.commandID.String()})
		m.addNotice(Notice{CommandID: msg.commandID, Severity: NoticeError, Text: "query failed: " + msg.err.Error(), Sticky: true})
		return
	}
	text := strings.TrimSpace(msg.report.Text)
	if text == "" {
		text = "(empty query result)"
	}
	// A query is read-only, so a late result is still useful, but make its
	// revision boundary explicit instead of allowing an async report to look
	// like current mutable state.
	if m.hasSnapshot && msg.report.Revision.LogSeq != 0 && msg.report.Revision.LogSeq < m.snapshot.Revision.LogSeq {
		text = fmt.Sprintf("[report from revision %d; current revision %d]\n%s", msg.report.Revision.LogSeq, m.snapshot.Revision.LogSeq, text)
	}
	m.addReport(historyCell{kind: "system", text: text, reportID: msg.commandID.String()})
}

// openWatchCmd establishes a new terminal subscription. Watch itself emits
// an atomic initial snapshot, so resync recovery never has a snapshot/watch
// race. Connection recovery retries observation only, never submissions.
func (m Model) openWatchCmd() tea.Cmd {
	client := m.client
	if client == nil {
		return nil
	}
	ctx := m.watchCtx
	if ctx == nil {
		ctx = context.Background()
	}
	sid := protocol.SessionID(m.sessionID)
	generation := m.watchGeneration
	cursor := m.watchCursor
	return func() tea.Msg {
		sub, err := client.Watch(ctx, cursor)
		return watchOpenedMsg{sub: sub, sessionID: sid, generation: generation, err: err}
	}
}

func readWatchCmd(sub protocol.Subscription, sid protocol.SessionID, generation uint64) tea.Cmd {
	if sub == nil {
		return nil
	}
	return func() tea.Msg {
		update, ok := <-sub.Updates()
		if !ok {
			return watchClosedMsg{sub: sub, sessionID: sid, generation: generation}
		}
		return watchUpdateMsg{sub: sub, update: update, sessionID: sid, generation: generation}
	}
}

func (m *Model) handleWatchOpened(msg watchOpenedMsg) (tea.Model, tea.Cmd) {
	if msg.generation != m.watchGeneration || msg.sessionID.String() != m.sessionID {
		if msg.sub != nil {
			_ = msg.sub.Close()
		}
		return m, nil
	}
	if msg.err != nil {
		m.pushStatus("watch unavailable: " + msg.err.Error())
		return m, m.scheduleReconnect()
	}
	m.subscription = msg.sub
	return m, readWatchCmd(msg.sub, msg.sessionID, msg.generation)
}

func (m *Model) handleWatchClosed(msg watchClosedMsg) (tea.Model, tea.Cmd) {
	if msg.generation != m.watchGeneration || msg.sessionID.String() != m.sessionID {
		return m, nil
	}
	if m.subscription != msg.sub {
		return m, nil
	}
	m.subscription = nil
	return m, m.scheduleReconnect()
}

func (m *Model) handleWatchUpdate(msg watchUpdateMsg) (tea.Model, tea.Cmd) {
	if msg.generation != m.watchGeneration || msg.sessionID.String() != m.sessionID {
		return m, nil
	}
	if msg.sub != m.subscription {
		return m, nil
	}
	u := msg.update
	m.watchCursor = u.Cursor
	if u.Revision.LogSeq > m.watchCursor.LogSeq {
		m.watchCursor.LogSeq = u.Revision.LogSeq
	}
	if u.Type == protocol.UpdateResyncRequired {
		_ = msg.sub.Close()
		m.subscription = nil
		m.watchGeneration++
		return m, m.openWatchCmd()
	}
	if u.Child != nil {
		// The root stream multiplexes each child's current session row. Fold it
		// into the directory so the parent's Task marker tracks the child live;
		// the child's own transcript never enters the parent conversation.
		m.applyChildUpdate(u.Child.Session)
	}
	if u.Snapshot != nil && (u.Type == protocol.UpdateSnapshot || u.Type == protocol.UpdateState) {
		m.watchDisconnected = false
		m.watchRetry = 0
		m.applySnapshot(*u.Snapshot)
	}
	if u.Receipt != nil && u.Type == protocol.UpdateReceipt {
		m.applyReceipt(*u.Receipt, "")
	}
	if u.Event != nil {
		m.handleProtocolEvent(*u.Event)
	}
	resetNativeHistory := m.nativeHistoryResetPending
	m.nativeHistoryResetPending = false
	m.renderVisibleTranscript()
	cmds := []tea.Cmd{readWatchCmd(msg.sub, msg.sessionID, msg.generation), m.flushInline()}
	if resetNativeHistory {
		// The erase sequence is written before the renderer repaints, so the
		// discarded conversation cannot reappear in a later frame.
		cmds = append(cmds, m.eraseNativeHistory())
	}
	return m, tea.Batch(cmds...)
}

// handleProtocolEvent updates transient presentation only. Confirmed model,
// mode, sandbox and plan state continues to come from snapshots.
func (m *Model) handleProtocolEvent(ev protocol.EventView) {
	if ev.SessionID != "" && ev.SessionID.String() != m.sessionID {
		return
	}
	// Project the event into the activity lane before handling transcript data.
	// The transcript path can return early for a typed stream/tool update, but
	// its phase still needs to be visible to the renderer.
	m.updateActivityFromEvent(ev)
	m.associateInputOperationFromEvent(ev)
	if ev.Transcript != nil && ev.Transcript.ID != "" {
		switch ev.Kind {
		case protocol.EventStream:
			m.upsertTranscript(*ev.Transcript)
			if ev.Text == "" {
				// Reasoning-close signal: it finalizes the thinking cell, it is
				// not answer text, so it must not (re)mark the turn as streaming.
				m.streaming = false
				return
			}
			m.streaming, m.turnDone = true, false
			return
		case protocol.EventReasoning, protocol.EventToolStarted, protocol.EventToolProgress, protocol.EventToolResult:
			m.upsertTranscript(*ev.Transcript)
			m.streaming = false
			return
		}
	}
	switch ev.Kind {
	case protocol.EventUserMessage:
		if !m.inline.primed {
			m.inline.prime(m.items)
		}
		if ev.MessageID != "" {
			m.upsertTranscript(protocol.TranscriptItem{ID: ev.MessageID, Kind: "user", TurnID: ev.TurnID, Text: ev.Text, Status: "completed"})
		}
		m.busy, m.streaming, m.turnDone = true, false, false
		m.turnFailed, m.interruptRequested = false, false
		m.turnStarted = now()
	case protocol.EventStream, protocol.EventReasoning, protocol.EventToolStarted, protocol.EventToolProgress:
		// Transcript events must carry an identified cumulative projection.
		// Missing projections never create a second, id-less transcript.
		return
	case protocol.EventToolResult:
		operationReport := false
		if ev.Tool != nil {
			_, operationReport = m.operationReports[protocol.CommandID(ev.Tool.ID.String())]
		}
		if ev.Tool != nil && (strings.HasPrefix(ev.Tool.Name, "query:") || operationReport) {
			// Typed operation/query commands use the generic operation event bridge.
			// Promote their bounded output to a permanent report rather than a
			// transient tool row that a later snapshot would erase.
			kind := "system"
			if ev.Tool.Status == "error" || ev.Tool.Status == "denied" {
				kind = "error"
			}
			text := strings.TrimSpace(ev.Tool.Output)
			if text == "" {
				text = "(empty query result)"
			}
			m.addReport(historyCell{kind: kind, text: text, reportID: ev.Tool.ID.String()})
			delete(m.operationReports, protocol.CommandID(ev.Tool.ID.String()))
		}
	case protocol.EventStatus:
		if strings.TrimSpace(ev.Text) != "" {
			m.addNotice(Notice{Severity: NoticeInfo, Text: ev.Text, Sticky: false})
		}
	case protocol.EventError:
		text := ev.Error
		if text == "" {
			text = ev.Text
		}
		// The transcript owns the error explanation. A sticky notice with the
		// same body would repeat it indefinitely above the composer.
		if !m.hasTurnError(ev.TurnID, text) {
			id := ""
			if ev.TurnID != "" {
				id = fmt.Sprintf("error-event:%s:%x", canonicalUITurn(ev.TurnID), sha256.Sum256([]byte(text)))
			}
			m.addReport(historyCell{kind: "error", text: text, turnID: ev.TurnID, reportID: id})
		}
		m.turnFailed = true
	case protocol.EventApprovalRequest:
		if ev.Approval != nil {
			m.clearDecisionNotices()
			m.approval = approvalRequestFromView(ev.Approval)
			m.approvalState.Offset = 0
			m.addNotice(Notice{Severity: NoticeDecision, Text: "awaiting approval…", Sticky: true})
			m.notifyUser("ccdp needs your decision", ev.Approval.Tool)
		}
	case protocol.EventQuestionRequest:
		m.setQuestion(ev.Question)
		if ev.Question != nil {
			m.notifyUser("ccdp has a question", "waiting for your answer")
		}
	case protocol.EventPlanReady:
		if ev.Plan != nil {
			m.clearDecisionNotices()
			m.addReport(historyCell{kind: "system", text: "── plan proposed ──"})
			m.addReport(historyCell{kind: "assistant", text: ev.Plan.Text})
			m.approval = &approvalPrompt{ID: ev.Plan.ID, Tool: "Plan", Command: ev.Plan.Text, Reason: "Approve this plan and start executing?"}
			m.approvalState.Offset = 0
			m.addNotice(Notice{Severity: NoticeDecision, Text: "awaiting plan approval…", Sticky: true})
			m.notifyUser("ccdp needs your decision", "a plan is ready to approve")
		}
	case protocol.EventUsage:
		if ev.Usage != nil {
			m.usage = protocol.UsageSnapshot{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens, CachedTokens: ev.Usage.CachedTokens, Cache: ev.Usage.Cache, Cost: ev.Usage.Cost, TurnCount: ev.Usage.TurnCount}
		}
	case protocol.EventTurnDone:
		outcome := protocol.TurnSucceeded
		if m.turnFailed {
			outcome = protocol.TurnFailed
		} else if m.interruptRequested {
			outcome = protocol.TurnCancelled
		}
		m.finishInputOperations(ev.TurnID, outcome, ev.Error)
		m.busy, m.streaming, m.turnDone = false, false, true
		m.clearTransientNotices()
		m.finishActivity("", ActivityCompleted, "")
		m.turnFailed = m.turnFailed || m.interruptRequested
		if !m.turnStarted.IsZero() && now().Sub(m.turnStarted) > 15_000_000_000 {
			m.notifyUser("ccdp finished", "a turn completed")
		}
	case protocol.EventStateChanged:
		// The accompanying state snapshot is the authority. Do not mutate
		// confirmed settings from an event that may be stale.
	}
}

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
		matches := candidate.InputText == ev.Text
		if candidate.InputID != "" && ev.MessageID != "" {
			matches = protocol.InputMessageID(candidate.InputID) == ev.MessageID
		}
		// Older events without message identity use the legacy text fallback.
		// Identity-bearing events must never match another input by text,
		// including image-only submissions whose text is expanded at runtime.
		if !matches && candidate.InputText == "" && ev.MessageID == "" {
			matches = true
		}
		if !matches {
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
			matches := candidate.InputText == item.Text
			if candidate.InputID != "" && item.ID != "" {
				matches = protocol.InputMessageID(candidate.InputID) == item.ID
			}
			if candidate.TurnID != "" || !matches {
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
		state, phase := OperationApplied, ActivityCompleted
		switch outcome {
		case protocol.TurnFailed:
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
		if op.CommandID == m.retryCommandID {
			m.retryCommandID, m.retryDraft, m.retryImages = "", "", nil
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
				if state == OperationCancelled || !m.hasTurnError(turnID, detail) {
					m.addNotice(Notice{CommandID: op.CommandID, Severity: NoticeError, Text: message, Sticky: true})
				}
			}
		}
	}
}

func approvalRequestFromView(view *protocol.ApprovalView) *approvalPrompt {
	if view == nil {
		return nil
	}
	command := ""
	if len(view.Args) > 0 {
		var object map[string]any
		if json.Unmarshal(view.Args, &object) == nil {
			if raw, ok := object["command"].(string); ok {
				command = raw
			}
			if command == "" {
				command = string(view.Args)
			}
		} else {
			command = string(view.Args)
		}
	}
	return &approvalPrompt{ID: view.ID, Tool: view.Tool, Command: command, Reason: view.Reason,
		Capabilities: append([]protocol.CapabilityRequest(nil), view.Capabilities...)}
}

// Small indirections keep protocol projection testable without binding the
// event path to wall-clock or process-global stdout in tests.
var now = func() time.Time { return time.Now() }

func protocolToolArgs(t *protocol.ToolView) map[string]any {
	if t == nil || len(t.Args) == 0 {
		return nil
	}
	var args map[string]any
	_ = json.Unmarshal(t.Args, &args)
	return args
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
		m.restoreFailedSubmission(receipt.CommandID)
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
			m.activity.Phase = ActivityQueued
			m.activity.Label = "已排队"
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
	if receipt.CommandID == m.retryCommandID {
		m.retryCommandID = ""
		m.retryDraft = ""
		m.retryImages = nil
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

// restoreFailedSubmission handles a terminal submission failure, whether it
// arrived as a transport error or a rejected receipt. There is no pending
// runtime operation to await after either outcome, so its marker is removed
// before the UI is made resumable.
func (m *Model) restoreFailedSubmission(commandID protocol.CommandID) {
	op, ok := m.operations[commandID]
	if !ok || !op.Recoverable {
		return
	}
	m.consumeSubmission(commandID)
	m.restoreSubmissionDraft(commandID, op.InputText, op.InputImages)
}

func (m *Model) restoreSubmissionDraft(commandID protocol.CommandID, text string, images []protocol.InputImage) {
	if text == "" && len(images) == 0 {
		return
	}
	// Keep the failed intent independently from the in-flight map. This also
	// preserves it when the user has already started drafting the next input.
	m.retryCommandID = commandID
	m.retryDraft = text
	m.retryImages = cloneInputImages(images)
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

// commandForPolicy returns a copy of the confirmed sandbox/permission policy
// so a pending mutation cannot modify a snapshot retained by the Model.
func (m Model) sandboxPolicy() protocol.SandboxPolicy {
	if m.hasSnapshot {
		p := m.snapshot.Settings.Sandbox
		p.AdditionalDirectories = append([]string(nil), p.AdditionalDirectories...)
		p.AdditionalReadOnlyDirectories = append([]string(nil), p.AdditionalReadOnlyDirectories...)
		p.DisallowedDirectories = append([]string(nil), p.DisallowedDirectories...)
		return p
	}
	return protocol.SandboxPolicy{}
}

func (m Model) permissionPolicy() protocol.PermissionPolicy {
	if m.hasSnapshot {
		p := m.snapshot.Settings.Permission
		p.AlwaysAllow = append([]string(nil), p.AlwaysAllow...)
		p.AlwaysDeny = append([]string(nil), p.AlwaysDeny...)
		return p
	}
	return protocol.PermissionPolicy{Mode: string(m.mode)}
}

// cyclePermissionMode advances the permission mode through the approval-strictness
// ring on shift+tab, mirroring the sibling CLIs. Plan is deliberately excluded: it
// is an execution-mode dimension toggled by /plan, not a permission level, so a
// mode outside the ring restarts at default. The change applies immediately; the
// local mode updates optimistically and the next snapshot confirms it.
func (m *Model) cyclePermissionMode() tea.Cmd {
	// m.mode is refreshed from every authoritative snapshot, so cycling off it
	// keeps rapid shift+tab presses chained correctly even before the round-trip
	// for the previous change lands. Only fall back to the snapshot when no mode
	// has been resolved locally yet.
	current := m.mode
	if current == "" && m.hasSnapshot {
		current = permissions.Mode(m.snapshot.Settings.Permission.Mode)
	}
	ring := []permissions.Mode{permissions.ModeDefault, permissions.ModeAcceptEdits, permissions.ModeBypass}
	next := ring[0]
	for i, mode := range ring {
		if mode == current {
			next = ring[(i+1)%len(ring)]
			break
		}
	}
	policy := m.permissionPolicy()
	policy.Mode = string(next)
	m.mode = next
	return m.submitCommand(protocol.Command{Type: protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}, "permission mode → "+next.Label())
}

// modeActive reports whether a non-default permission mode is confirmed, so the
// footer can advertise the shift+tab cycle hint only when it is relevant.
func (m *Model) modeActive() bool {
	mode := m.mode
	if m.hasSnapshot && m.snapshot.Settings.Permission.Mode != "" {
		mode = permissions.Mode(m.snapshot.Settings.Permission.Mode)
	}
	return mode != "" && mode != permissions.ModeDefault
}

func (m Model) availableModels() []string {
	if m.hasSnapshot {
		seen := make(map[string]bool)
		var models []string
		for _, provider := range m.snapshot.Catalog.Providers {
			for _, model := range provider.Models {
				if model != "" && !seen[model] {
					seen[model] = true
					models = append(models, model)
				}
			}
		}
		if len(models) > 0 {
			sort.Strings(models)
			return models
		}
	}
	if m.modelName != "" {
		return []string{m.modelName}
	}
	return nil
}

func (m Model) historyPreview() (int, []string) {
	if !m.hasSnapshot {
		return 0, nil
	}
	previews := make([]string, 0, len(m.snapshot.History))
	for _, message := range m.snapshot.History {
		text := strings.TrimSpace(message.Content)
		if text == "" {
			text = "(empty message)"
		}
		if len(text) > 120 {
			text = text[:120] + "…"
		}
		previews = append(previews, message.Role+": "+text)
	}
	return len(m.snapshot.History), previews
}

func (m *Model) executeSelectorAction(action selectorAction, optionID string) tea.Cmd {
	switch action.Kind {
	case selectorEffort, selectorVerbosity:
		value := optionID
		if value == "default" {
			value = ""
		}
		generation := &protocol.SetGeneration{}
		if action.Kind == selectorEffort {
			generation.ReasoningEffort = &value
		} else {
			generation.Verbosity = &value
		}
		return m.submitCommand(protocol.Command{Type: protocol.CommandSetGeneration, Generation: generation}, string(action.Kind)+" → "+optionID)
	case selectorAgent:
		return m.openAgentView(optionID)
	case selectorAgentOutput:
		return m.agentOutputSelection(optionID)
	case selectorModel:
		return m.submitCommand(protocol.Command{Type: protocol.CommandSetModel,
			Model: &protocol.SetModel{Model: optionID}}, "model → "+optionID)
	case selectorMode:
		policy := m.permissionPolicy()
		policy.Mode = optionID
		return m.submitCommand(protocol.Command{Type: protocol.CommandSetPermissionPolicy,
			PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}, "permission mode → "+permissions.Mode(optionID).Label())
	case selectorPlan:
		mode := protocol.ExecutionModeExecute
		if optionID == "on" || optionID == string(protocol.ExecutionModePlan) {
			mode = protocol.ExecutionModePlan
		}
		return m.submitCommand(protocol.Command{Type: protocol.CommandSetExecutionMode,
			ExecutionMode: &protocol.SetExecutionMode{Mode: mode}}, "plan mode → "+optionID)
	case selectorRewind:
		keep, ok := parseRewindOption(optionID)
		if !ok {
			return nil
		}
		return m.submitCommand(protocol.Command{Type: protocol.CommandRewindConversation,
			Rewind: &protocol.RewindConversation{Count: keep}}, fmt.Sprintf("rewind to message %d submitted", keep))
	case selectorResume:
		return m.openResumeSession(optionID)
	}
	return nil
}

func (m *Model) pendingChildApproval() *protocol.ChildSession {
	if m == nil || m.routing == nil {
		return nil
	}
	for i := range m.routing.rows {
		if childAwaitingApproval(m.routing.rows[i]) {
			return &m.routing.rows[i]
		}
	}
	return nil
}

func parseRewindOption(id string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "keep:"))
	return n, err == nil && strings.HasPrefix(id, "keep:") && n >= 0
}
