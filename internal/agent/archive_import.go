package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// memoryResumeEvents maps a caller-owned snapshot to typed facts while
// retaining existing message IDs and timestamps. This is the explicit in-memory resume path; disk sessions replay their JSONL log.
func memoryResumeEvents(snapshot SessionSnapshot) ([]session.Event, error) {
	_, importedAt := resumeTimestamps(snapshot)
	events := make([]session.Event, 0, len(snapshot.History)*2+len(snapshot.PendingInputs)+3)
	events = appendResumeStateFacts(events, snapshot)
	if err := appendResumeHistoryEvents(&events, snapshot); err != nil {
		return nil, err
	}
	events = appendResumePendingInputs(events, snapshot, importedAt)
	events = append(events, session.SessionImported{
		SessionID: snapshot.ID, FormatVersion: session.SchemaVersion, Source: "memory-resume", ImportedAt: importedAt,
		ParentID: snapshot.ParentID, BranchPoint: snapshot.BranchPoint, BranchSummary: snapshot.BranchSummary,
	})
	return events, nil
}

// resumeTimestamps reconciles the snapshot's creation and import timestamps.
func resumeTimestamps(snapshot SessionSnapshot) (createdAt, importedAt time.Time) {
	createdAt = snapshot.CreatedAt
	if createdAt.IsZero() {
		createdAt = snapshot.UpdatedAt
	}
	if createdAt.IsZero() {
		createdAt = time.Unix(0, 0).UTC()
	}
	importedAt = snapshot.UpdatedAt
	if importedAt.IsZero() {
		importedAt = createdAt
	}
	return createdAt, importedAt
}

// appendResumeStateFacts restores workspace/model/usage/memory/tasks/workflow
// state as durable facts.
func appendResumeStateFacts(events []session.Event, snapshot SessionSnapshot) []session.Event {
	if snapshot.Workspace != "" || snapshot.Model != "" {
		events = append(events, session.SettingsChanged{Revision: 1, Settings: session.Settings{Workspace: snapshot.Workspace, Model: snapshot.Model}})
	}
	if snapshot.Usage != (Usage{}) {
		events = append(events, session.UsageChanged{Revision: 1, Usage: session.Usage{
			InputTokens: int64(snapshot.Usage.InputTokens), OutputTokens: int64(snapshot.Usage.OutputTokens),
			CachedTokens: int64(snapshot.Usage.CachedTokens), Cache: snapshot.Usage.Cache, TotalTokens: int64(snapshot.Usage.InputTokens + snapshot.Usage.OutputTokens),
			TurnCount: int64(snapshot.Usage.TurnCount),
		}})
	}
	if snapshot.Settings != nil {
		events = append(events, session.SettingsChanged{Revision: 1, Settings: *snapshot.Settings})
	}
	for _, command := range snapshot.PendingCommands {
		// The command payload is intentionally absent from the snapshot.  A
		// resumed no-session runtime can therefore expose the admitted operation
		// as unknown, but can never reconstruct and re-run its side effect.
		events = append(events, command)
	}
	if snapshot.Goal != nil {
		events = append(events, session.GoalChanged{Goal: protocol.CloneGoal(snapshot.Goal)})
		ids := make([]string, 0, len(snapshot.GoalAccountingIDs))
		for id, accounted := range snapshot.GoalAccountingIDs {
			if accounted {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			events = append(events, session.GoalChanged{Goal: protocol.CloneGoal(snapshot.Goal), AccountingID: id})
		}
	}
	if snapshot.Workflow != nil {
		events = append(events, session.WorkflowChanged{Workflow: *snapshot.Workflow})
	}
	if snapshot.Memory != "" {
		events = append(events, session.MemoryChanged{Revision: 1, Text: snapshot.Memory})
	}
	if len(snapshot.Tasks) > 0 {
		events = append(events, session.TasksChanged{Revision: 1, Tasks: append([]session.Task(nil), snapshot.Tasks...)})
	}
	return events
}

// appendResumeHistoryEvents preserves complete messages using the same typed
// history fact used by fork initialization, including user images and metadata.
func appendResumeHistoryEvents(events *[]session.Event, snapshot SessionSnapshot) error {
	for i, original := range snapshot.History {
		message := original
		if message.ID == "" {
			message.ID = snapshotMessageID(snapshot.ID, i)
		}
		converted, err := messageToSession(message)
		if err != nil {
			return fmt.Errorf("memory resume history[%d]: %w", i, err)
		}
		*events = append(*events, session.AssistantCommitted{TurnID: fmt.Sprintf("memory-resume-turn-%06d", i), Message: converted, ToolCalls: session.MessageToolCalls(converted)})
	}
	return nil
}

// appendResumePendingInputs restores queued inputs as InputQueued facts.
func appendResumePendingInputs(events []session.Event, snapshot SessionSnapshot, importedAt time.Time) []session.Event {
	for _, input := range snapshot.PendingInputs {
		if (strings.TrimSpace(input.Text) == "" && len(snapshot.PendingAttachments[string(input.ID)]) == 0) || input.ID == "" {
			continue
		}
		strategy := input.Strategy
		if strategy != protocol.InputSteer && strategy != protocol.InputFollowup && strategy != protocol.InputMessage {
			strategy = protocol.InputFollowup
		}
		created := input.CreatedAt
		if created.IsZero() {
			created = importedAt
		}
		var attachments []session.BlobRef
		for _, image := range snapshot.PendingAttachments[string(input.ID)] {
			attachments = append(attachments, session.BlobRef{Hash: image.BlobHash, Size: image.BlobSize, MediaType: image.MediaType})
		}
		events = append(events, session.InputQueued{
			InputID: string(input.ID), MessageID: inputMessageID(string(input.ID)), Text: input.Text,
			Strategy: string(strategy), CreatedAt: created, Attachments: attachments, ImagesFrozen: true,
		})
	}
	return events
}
