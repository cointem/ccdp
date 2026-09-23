package tui

import (
	"ccdp/internal/agent"
	"ccdp/internal/protocol"
	"context"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"sync"
)

// sessionHost is the desktop runtime lifecycle adapter. SessionClient remains
// the only interaction boundary; runtime handles stay here for open/close and
// the CLI shutdown handoff.
type sessionHost struct {
	ag *agent.Agent
	// resumePending gates new submissions while an independent session handle
	// is being opened. This prevents a late successful open from closing the
	// handle that accepted a newly submitted turn.
	resumePending bool
	resumeTask    *resumeTaskState
	retiredAgents []*agent.Agent
}

func (h *sessionHost) savedSessions() ([]agent.SessionSnapshot, []agent.SessionListIssue, error) {
	if h.ag == nil {
		return nil, nil, fmt.Errorf("saved sessions unavailable through this session protocol")
	}
	return agent.ListSessions(h.ag.SessionDir())
}

// resumeOpenedMsg is produced after an independent Agent handle has been
// opened. The source scope is carried through the asynchronous operation so a
// late result cannot replace a session that the user has already changed.
type resumeOpenedMsg struct {
	candidate          *agent.Agent
	snapshot           protocol.SessionView
	source             *agent.Agent
	task               *resumeTaskState
	expectedSessionID  string
	expectedGeneration uint64
	err                error
}

type resumeCandidateClosedMsg struct{}

// resumeTaskState outlives Bubble Tea's value Model copies. It lets shutdown
// join an OpenSession operation that has started, or cancel a command that was
// queued but never run, and close an unclaimed candidate exactly once.
type resumeTaskState struct {
	mu        sync.Mutex
	done      chan struct{}
	started   bool
	canceled  bool
	finished  bool
	claimed   bool
	candidate *agent.Agent
}

// NewWithResume builds the TUI model and optionally opens the resume picker.
func NewWithResume(ag *agent.Agent, autoResume bool) Model {
	var client protocol.SessionClient
	if ag != nil {
		client = ag
	}
	workspace := ""
	if ag != nil {
		workspace = ag.WorkspaceLabel()
	}
	m := NewWithClient(client, workspace, autoResume)
	m.ag = ag
	if ag != nil {
		ag.SetChildInteraction(true)
		m.installSessionRouting(ag.Sessions())
	}
	m.inline.prime(m.items)
	m.inline.showInitialFrame()
	return m
}

func (m *Model) Close() error {
	var closeErr error
	if m.terminal != nil {
		closeErr = m.terminal.close()
	}
	if m.watchCancel != nil {
		m.watchCancel()
	}
	if m.subscription != nil {
		closeErr = errors.Join(closeErr, m.subscription.Close())
		m.subscription = nil
	}
	if m.resumeTask != nil {
		m.resumeTask.cancelIfNotStarted()
		m.resumeTask.waitAndCloseUnclaimed()
	}
	for _, retired := range m.retiredAgents {
		if retired != nil {
			retired.Close()
		}
	}
	m.retiredAgents = nil
	return closeErr
}

// CurrentAgent returns the live runtime handle currently owned by the TUI.
// It is used by the CLI shutdown path after a resume/fork session swap so the
// final session, rather than the original startup handle, is saved and closed.
func (m Model) CurrentAgent() *agent.Agent { return m.ag }

func (t *resumeTaskState) begin() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.canceled {
		return false
	}
	t.started = true
	return true
}

func (t *resumeTaskState) finish(candidate *agent.Agent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if candidate != nil {
		t.candidate = candidate
	}
	if !t.finished {
		t.finished = true
		close(t.done)
	}
}

func (t *resumeTaskState) cancelIfNotStarted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return false
	}
	t.canceled = true
	if !t.finished {
		t.finished = true
		close(t.done)
	}
	return true
}

func (t *resumeTaskState) waitAndCloseUnclaimed() {
	t.mu.Lock()
	started := t.started
	done := t.done
	t.mu.Unlock()
	if started {
		<-done
	}
	t.mu.Lock()
	candidate := t.candidate
	claimed := t.claimed
	if candidate != nil && !claimed {
		t.claimed = true
	}
	t.mu.Unlock()
	if candidate != nil && !claimed {
		candidate.Close()
	}
}

func (t *resumeTaskState) claim() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.claimed = true
	t.mu.Unlock()
}

func (m *Model) openResumeSession(id string) tea.Cmd {
	if m.routing != nil && m.sessionID != m.routing.rootID {
		m.pushStatus("return to /root before resuming another root session")
		return nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		m.pushLog("error", "resume requires a session id")
		return nil
	}
	source := m.ag
	if source == nil {
		m.pushLog("error", "resume requires an agent session opener")
		return nil
	}
	if m.resumePending {
		m.pushStatus("resume is already opening; please wait")
		return nil
	}
	if m.busy || m.pendingSubmissionCount() > 0 {
		m.pushStatus("resume is unavailable while a turn or submission is active")
		return nil
	}
	expectedSessionID := m.sessionID
	expectedGeneration := m.watchGeneration
	task := &resumeTaskState{done: make(chan struct{})}
	m.resumeTask = task
	m.resumePending = true
	m.pushStatus("opening session " + id + "…")
	return func() tea.Msg {
		if !task.begin() {
			task.finish(nil)
			return resumeOpenedMsg{source: source, task: task, expectedSessionID: expectedSessionID,
				expectedGeneration: expectedGeneration, err: context.Canceled}
		}
		candidate, err := source.OpenSession(id)
		if err != nil {
			task.finish(nil)
			return resumeOpenedMsg{source: source, expectedSessionID: expectedSessionID,
				task: task, expectedGeneration: expectedGeneration, err: err}
		}
		if candidate == nil {
			task.finish(nil)
			return resumeOpenedMsg{source: source, expectedSessionID: expectedSessionID,
				task: task, expectedGeneration: expectedGeneration, err: fmt.Errorf("session opener returned a nil handle")}
		}
		snapshot, snapshotErr := candidate.Snapshot(context.Background())
		if snapshotErr != nil {
			candidate.Close()
			task.finish(candidate)
			return resumeOpenedMsg{source: source, expectedSessionID: expectedSessionID,
				task: task, expectedGeneration: expectedGeneration, err: snapshotErr}
		}
		task.finish(candidate)
		return resumeOpenedMsg{candidate: candidate, snapshot: snapshot, source: source,
			task: task, expectedSessionID: expectedSessionID, expectedGeneration: expectedGeneration}
	}
}

func (m *Model) handleResumeOpened(msg resumeOpenedMsg) (tea.Model, tea.Cmd) {
	valid := msg.expectedGeneration == m.watchGeneration &&
		msg.expectedSessionID == m.sessionID && msg.source == m.ag
	if !valid {
		if msg.source == m.ag {
			m.resumePending = false
		}
		if msg.candidate != nil {
			return m, closeResumeCandidateCmd(msg.candidate)
		}
		return m, nil
	}
	m.resumePending = false
	if msg.err != nil {
		m.pushLog("error", "resume failed: "+msg.err.Error())
		return m, nil
	}
	if msg.candidate == nil {
		m.pushLog("error", "resume failed: session opener returned no handle")
		return m, nil
	}
	if m.busy || m.pendingSubmissionCount() > 0 {
		m.pushStatus("resume cancelled: a turn or submission became active")
		return m, closeResumeCandidateCmd(msg.candidate)
	}
	if msg.task != nil {
		msg.task.claim()
	}

	old := m.ag
	if m.watchCancel != nil {
		m.watchCancel()
	}
	if m.subscription != nil {
		_ = m.subscription.Close()
	}
	m.subscription = nil
	m.watchGeneration++
	// OpenSession succeeded and the request is still in the original scope;
	// it is now safe to release the old handle. A stale result above closes the
	// candidate instead and leaves this session untouched.
	var cleanup tea.Cmd
	if old != nil && old != msg.candidate {
		m.retiredAgents = append(m.retiredAgents, old)
		cleanup = closeResumeCandidateCmd(old)
	}

	m.ag = msg.candidate
	m.client = msg.candidate
	m.watchCtx, m.watchCancel = context.WithCancel(context.Background())
	// A resumed handle is a new UI attachment even when the persisted session
	// ID is unchanged.  Clear inline identities before rebuilding the
	// authoritative history; otherwise the old attachment's printed IDs make
	// restored messages disappear from the native transcript.
	m.inline.forgetAll()
	m.watchCursor = protocol.Cursor{}
	m.snapshot = protocol.SessionView{}
	m.hasSnapshot = false
	m.sessionID = msg.snapshot.SessionID.String()
	if m.sessionID == "" {
		m.sessionID = msg.candidate.SessionID()
	}
	m.reportGeneration++
	m.operations = nil
	m.inputImages = nil
	m.retryImages = nil
	m.historyImages = nil
	m.expiredHistoryImages = nil
	m.missingHistoryImages = false
	m.pasteRequest = ""
	m.receiptKeys = nil
	m.receiptOrder = nil
	m.retryCommandID = ""
	m.retryDraft = ""
	m.pendingApprovalCommand = ""
	m.pendingApprovalKey = ""
	m.watchDisconnected = false
	m.watchRetry = 0
	m.exitConfirm = false
	m.approval = nil
	m.approvalPending = false
	m.confirmedItems = nil
	m.reports = nil
	m.items = nil
	m.streaming = false
	m.busy = false
	m.workspace = msg.candidate.WorkspaceLabel()
	m.customCmds = loadCustomCommands(m.workspace)
	m.applySnapshot(msg.snapshot)
	// applySnapshot cannot infer attachment boundaries from the session ID
	// alone.  Prime and show the restored baseline explicitly so it is emitted
	// once on the first real window/flush, including same-session resumes.
	m.inline.prime(m.items)
	m.inline.showInitialFrame()
	m.pushStatus("resumed session " + m.sessionID)
	m.ag.SetChildInteraction(true)
	m.installSessionRouting(m.ag.Sessions())
	return m, tea.Batch(cleanup, m.openWatchCmd(), m.loadAgentCatalog(false))
}

func closeResumeCandidateCmd(candidate *agent.Agent) tea.Cmd {
	if candidate == nil {
		return nil
	}
	return func() tea.Msg {
		candidate.Close()
		return resumeCandidateClosedMsg{}
	}
}

// sessionListIssuesNote renders the sessions a listing had to leave out. It
// returns "" when the listing was complete, so callers can tell an empty
// session directory apart from one they could not read.
func sessionListIssuesNote(issues []agent.SessionListIssue) string {
	if len(issues) == 0 {
		return ""
	}
	const maxListed = 3
	note := fmt.Sprintf("%d session(s) could not be read:", len(issues))
	for i, issue := range issues {
		if i == maxListed {
			note += fmt.Sprintf("\n  …and %d more", len(issues)-maxListed)
			break
		}
		note += fmt.Sprintf("\n  %s: %s", issue.ID, issue.Reason)
	}
	return note
}
