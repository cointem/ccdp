package archive

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
	"ccdp/internal/session/replay"
)

// Snapshot is the serializable form of a session.
type Snapshot struct {
	Records           []session.Record       `json:"-"`
	Artifacts         journal.ArtifactReader `json:"-"`
	GoalAccountingIDs map[string]bool        `json:"goal_accounting_ids,omitempty"`
	Goal              *protocol.Goal         `json:"goal,omitempty"`
	ID                string                 `json:"id"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
	Workspace         string                 `json:"workspace"`
	Model             string                 `json:"model"`
	Title             string                 `json:"title,omitempty"` // derived from the first user message
	History           []messages.Message     `json:"history"`
	Usage             session.Usage          `json:"usage"`
	// Settings and Workflow are optional typed projections. They keep public
	// session state recoverable without putting credentials or opaque runtime
	// objects in the session projection.
	Settings *session.Settings `json:"settings,omitempty"`
	// PendingCommands records admitted asynchronous operations with no durable
	// completion yet. Resume exposes them as unknown; it never re-runs them.
	PendingCommands []session.CommandScheduled `json:"pending_commands,omitempty"`
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
	TurnSeq uint64 `json:"-"`
	StepSeq uint64 `json:"-"`
}

// AgentParent returns the owning chat for a delegated run. User-created
// forks also have ParentID, but acquire it from a SessionImported event.
func (s *Snapshot) AgentParent() string {
	if s.ParentID == "" {
		return ""
	}
	var parent string
	for _, record := range s.Records {
		switch e := record.Event.(type) {
		case *session.SessionCreated:
			if e.ParentID == "" {
				return ""
			}
			switch e.Source {
			case "task", "guardian", "review":
				return e.ParentID
			case "agent", "": // Older delegated sessions used the default source.
				parent = e.ParentID
			}
		case *session.SessionImported:
			if e.Source == "fork" {
				return ""
			}
		}
	}
	return parent
}

func FromRecords(records []session.Record, id string) (*Snapshot, error) {
	projection, err := replay.Records(records)
	if err != nil {
		return nil, err
	}
	if projection.ID == "" {
		projection.ID = id
	}
	if id != "" && projection.ID != id {
		return nil, fmt.Errorf("agent: replayed session id %q does not match requested id %q", projection.ID, id)
	}
	snapshot := fromProjection(projection, id)
	snapshot.Records = records
	return snapshot, nil
}

func fromProjection(projection *replay.State, id string) *Snapshot {
	if projection == nil {
		return nil
	}
	if projection.ID == "" {
		projection.ID = id
	}
	title := Title(projection.History)
	if title == "" && projection.Goal != nil {
		title = truncateRunes(strings.Join(strings.Fields(projection.Goal.Objective), " "), 60)
	}
	return &Snapshot{
		ID: projection.ID, CreatedAt: projection.CreatedAt, UpdatedAt: projection.UpdatedAt,
		Workspace: projection.Workspace, Model: projection.Model, Title: title,
		History: messages.Clone(projection.History), Usage: projection.Usage, PendingInputs: projection.PendingInputViews(), PendingAttachments: projection.PendingAttachments(),
		Settings:          SnapshotSettings(projection.Settings),
		Goal:              protocol.CloneGoal(projection.Goal),
		GoalAccountingIDs: maps.Clone(projection.GoalAccountingIDs),
		Tasks:             append([]session.Task(nil), projection.Tasks...),
		PendingCommands:   projection.PendingCommands(),
		ParentID:          projection.ParentID, BranchPoint: projection.BranchPoint, BranchSummary: projection.BranchSummary,
		TurnSeq: projection.TurnSeq, StepSeq: projection.StepSeq,
	}
}

func SnapshotSettings(settings session.Settings) *session.Settings {
	if reflect.DeepEqual(settings, session.Settings{}) {
		return nil
	}
	copy := settings.Clone()
	return &copy
}

// The old session-created transaction recorded only workspace, model and
// memory settings. Its two zero network flags did not describe a policy.
func (s *Snapshot) HasLegacyCreationSettings() bool {
	settings := s.Settings
	if settings == nil || settings.PermissionPolicy != "" || len(s.Records) < 2 {
		return false
	}
	legacy := session.Settings{
		Workspace: settings.Workspace, Model: settings.Model, ProjectMemoryEnabled: settings.ProjectMemoryEnabled,
		MemoryReadOnly: settings.MemoryReadOnly, MemoryHistory: settings.MemoryHistory,
	}
	if !reflect.DeepEqual(*settings, legacy) {
		return false
	}
	for _, r := range slices.Backward(s.Records) {
		if _, ok := r.Event.(*session.SettingsChanged); ok {
			return r.Seq == 2 && r.TransactionID == "session-created-"+s.ID
		}
	}
	return false
}
