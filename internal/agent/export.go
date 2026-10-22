package agent

import (
	"ccdp/internal/session"
	"ccdp/internal/session/archive"

	"ccdp/internal/messages"
)

// History returns a copy of the conversation history.
func (a *Agent) History() []messages.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return messages.Clone(a.historyLocked())
}

// ExportMarkdown uses the same durable execution-log projection as CLI replay.
func (a *Agent) ExportMarkdown() string {
	a.mu.Lock()
	snapshot := &archive.Snapshot{ID: a.conversation.sessionID, Model: a.services.cfg.Model, Workspace: a.services.cfg.Workspace, History: messages.Clone(a.historyLocked())}
	a.mu.Unlock()
	if p := a.persistenceHandle(); p != nil {
		records, err := p.Read(session.Beginning)
		if err != nil {
			return archive.Markdown(snapshot) + "\nExecution log unavailable: " + err.Error() + "\n"
		}
		snapshot.Records = records
	}
	return archive.Markdown(snapshot)
}
