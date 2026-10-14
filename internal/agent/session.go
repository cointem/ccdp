package agent

import (
	"errors"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
	"ccdp/internal/session/journal"
)

// SessionSnapshot is the serializable form of a session.
type SessionSnapshot struct {
	resumeRecords     []session.Record
	resumeArtifacts   journal.ArtifactReader
	GoalAccountingIDs map[string]bool    `json:"goal_accounting_ids,omitempty"`
	Goal              *protocol.Goal     `json:"goal,omitempty"`
	ID                string             `json:"id"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
	Workspace         string             `json:"workspace"`
	Model             string             `json:"model"`
	Title             string             `json:"title,omitempty"` // derived from the first user message
	History           []messages.Message `json:"history"`
	Usage             Usage              `json:"usage"`
	// Settings and Workflow are optional typed projections. They keep public
	// session state recoverable without putting credentials or opaque runtime
	// objects in the session projection.
	Settings *session.Settings `json:"settings,omitempty"`
	// PendingCommands records admitted asynchronous operations with no durable
	// completion yet. Resume exposes them as unknown; it never re-runs them.
	PendingCommands []session.CommandScheduled `json:"pending_commands,omitempty"`
	Workflow        *session.WorkflowState     `json:"workflow,omitempty"`
	Memory          string                     `json:"memory,omitempty"`
	Tasks           []session.Task             `json:"tasks,omitempty"`
	// PendingInputs is the durable typed inbox.
	PendingInputs []protocol.InputView `json:"pending_inputs,omitempty"`
	// PendingAttachments carries only immutable blob identities in snapshots;
	// the bytes are recovered from the session ArtifactStore by request
	// construction. It is deliberately separate from the public InputView.
	PendingAttachments map[string][]messages.ImageAttachment `json:"pending_attachments,omitempty"`
	// Lineage (pi's session tree): the session this one branched from, the
	// history index the branch starts at, and an LLM summary of the abandoned
	// direction.
	ParentID      string `json:"parent_id,omitempty"`
	BranchPoint   int    `json:"branch_point,omitempty"`
	BranchSummary string `json:"branch_summary,omitempty"`
	// Sequence counters are derived from the typed log at the resume boundary.
	// Resume derives the authoritative values from the owned event store
	// before admitting a new turn.
	turnSeq uint64
	stepSeq uint64
}

// cloneInputViews returns an independent copy of the typed inbox projection.
// InputView currently contains only value fields, but keeping the copy helper
// here makes the snapshot boundary explicit and prevents future slice fields
// from aliasing the live Agent state.
func cloneInputViews(src []protocol.InputView) []protocol.InputView {
	if src == nil {
		return nil
	}
	return append([]protocol.InputView(nil), src...)
}

// LoadSession replays the session log without mutating the session tree.
func LoadSession(dir, id string) (*SessionSnapshot, error) {
	snapshot, err := loadSessionSnapshot(dir, id)
	return snapshot, err
}

// OpenSession prepares an independent Agent handle for a saved session. It
// never mutates this Agent: the caller can atomically replace its UI handle
// only after OpenSession succeeds, then close the old handle.
func (a *Agent) OpenSession(id string) (*Agent, error) {
	if a == nil || a.services.cfg == nil {
		return nil, errors.New("agent: cannot open a session from a nil agent")
	}
	a.mu.Lock()
	busy, closing := a.turn.busy, a.lifecycle.closing
	// Clone all mutable config state while holding the Agent lock. The opened
	// handle must not share provider maps, policy slices, or session channels
	// with the source; a late command on the old handle must never be consumed
	// by the new session.
	cfg := cloneConfig(&a.services.baseCfg)
	a.mu.Unlock()
	if busy {
		return nil, errors.New("agent: cannot open a session while a turn is running")
	}
	if closing {
		return nil, errors.New("agent: cannot open a session while closing")
	}
	var snapshot *SessionSnapshot
	var err error
	if cfg.NoSessionPersistence {
		catalog := a.memorySessionCatalog()
		if err = a.retainMemorySession(catalog); err != nil {
			return nil, err
		}
		catalog.mu.Lock()
		snapshot = catalog.sessions[id]
		catalog.mu.Unlock()
	}
	if snapshot == nil {
		snapshot, err = LoadSession(cfg.SessionDir, id)
	}
	if err != nil {
		return nil, err
	}
	opened, err := Resume(&cfg, snapshot)
	if err == nil && cfg.NoSessionPersistence {
		opened.memorySessions = a.memorySessionCatalog()
	}
	return opened, err
}

func validateSessionID(id string) error { return archive.ValidateID(id) }

// SessionListIssue records one session directory ListSessions had to leave out.
// A damaged or unreadable log must not hide every other session, but the
// omission has to be visible: without it an empty list is indistinguishable
// from "nothing to resume".
type SessionListIssue struct {
	ID     string
	Reason string
}
