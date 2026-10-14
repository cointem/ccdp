package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
)

func snapshotFromArchive(s *archive.Snapshot) (*SessionSnapshot, error) {
	if s == nil {
		return nil, nil
	}
	usage, err := usageFromSession(s.Usage)
	if err != nil {
		return nil, err
	}
	return &SessionSnapshot{
		GoalAccountingIDs:  s.GoalAccountingIDs,
		Goal:               s.Goal,
		ID:                 s.ID,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
		Workspace:          s.Workspace,
		Model:              s.Model,
		Title:              s.Title,
		History:            s.History,
		Settings:           s.Settings,
		PendingCommands:    s.PendingCommands,
		Workflow:           s.Workflow,
		Memory:             s.Memory,
		Tasks:              s.Tasks,
		PendingInputs:      s.PendingInputs,
		PendingAttachments: s.PendingAttachments,
		ParentID:           s.ParentID,
		BranchPoint:        s.BranchPoint,
		BranchSummary:      s.BranchSummary,
		Usage:              usage, resumeRecords: s.Records, resumeArtifacts: s.Artifacts, turnSeq: s.TurnSeq, stepSeq: s.StepSeq,
	}, nil
}

func loadSessionSnapshot(dir, id string) (*SessionSnapshot, error) {
	s, err := archive.Load(dir, id)
	if err != nil {
		return nil, err
	}
	return snapshotFromArchive(s)
}

func snapshotFromRecords(records []session.Record, id string) (*SessionSnapshot, error) {
	s, err := archive.FromRecords(records, id)
	if err != nil {
		return nil, err
	}
	return snapshotFromArchive(s)
}

func ListSessions(dir string) ([]SessionSnapshot, []SessionListIssue, error) {
	snapshots, problems, err := archive.List(dir)
	if err != nil {
		return nil, nil, err
	}
	var out []SessionSnapshot
	var issues []SessionListIssue
	for _, p := range problems {
		issues = append(issues, SessionListIssue{ID: p.ID, Reason: p.Reason})
	}
	for _, s := range snapshots {
		v, err := snapshotFromArchive(&s)
		if err != nil {
			issues = append(issues, SessionListIssue{ID: s.ID, Reason: err.Error()})
			continue
		}
		out = append(out, *v)
	}
	return out, issues, nil
}

func ExportSessionMarkdown(s *SessionSnapshot) string {
	if s == nil {
		return ""
	}
	return archive.Markdown(&archive.Snapshot{ID: s.ID, Model: s.Model, Workspace: s.Workspace, History: s.History})
}

func sessionTitle(history []messages.Message) string { return archive.Title(history) }

func snapshotSettings(s session.Settings) *session.Settings { return archive.SnapshotSettings(s) }

func snapshotWorkflow(s session.WorkflowState) *session.WorkflowState {
	return archive.SnapshotWorkflow(s)
}
