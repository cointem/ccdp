package tui

import (
	"reflect"

	"time"

	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

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
	if workspace := snapshot.Settings.Workspace; workspace != "" && workspace != m.workspace {
		m.workspace = workspace
		m.reportGeneration++ // discard custom-command reads from the previous directory
		m.customCmds = loadCustomCommands(workspace)
		m.refreshFileMention()
		if m.reader != nil {
			m.reader.workspace, m.reader.cacheWidth = workspace, 0
		}
	}
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
	if snapshot.Busy {
		if !snapshot.WorkStartedAt.IsZero() {
			m.turnStarted = snapshot.WorkStartedAt
		} else if !m.busy || sessionChanged || m.turnStarted.IsZero() {
			m.turnStarted = now()
		}
	} else if sessionChanged {
		m.turnStarted = time.Time{}
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
		CachedTokens: snapshot.Usage.CachedTokens, Cache: snapshot.Usage.Cache,
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
	// Transcript previews can survive a reconnect; the runtime phase owns
	// whether those rows still represent an active stream.
	m.streaming = m.streaming && snapshot.Busy && snapshot.Phase == protocol.PhaseStreaming
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
