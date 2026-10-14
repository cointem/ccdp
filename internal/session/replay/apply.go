package replay

import (
	"fmt"
	"strings"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func (p *State) Apply(event session.Event) error {
	event = session.EventPointer(event)
	if event == nil {
		return fmt.Errorf("replay: nil event")
	}
	switch e := event.(type) {
	case *session.SessionCreated:
		p.ID, p.CreatedAt = e.SessionID, e.CreatedAt
		if e.ParentID != "" {
			p.ParentID = e.ParentID
		}
		p.UpdatedAt = e.CreatedAt
	case *session.SessionImported:
		if p.ID == "" {
			p.ID = e.SessionID
		}
		p.ParentID, p.BranchPoint, p.BranchSummary = e.ParentID, e.BranchPoint, e.BranchSummary
		if e.ImportedAt.After(p.UpdatedAt) {
			p.UpdatedAt = e.ImportedAt
		}
	case *session.SettingsChanged:
		// SettingsChanged is a frozen public session snapshot. Keep the full
		// value for resume/replay (credentials never belong in session.Settings)
		// while retaining the legacy Workspace/Model projection fields.
		p.Settings = e.Settings.Clone()
		p.SettingsRev = e.Revision
		p.Model, p.Workspace = e.Settings.Model, e.Settings.Workspace
	case *session.CommandScheduled:
		if prior, exists := p.CommandDigests[e.CommandID]; exists && prior != e.InputDigest {
			return fmt.Errorf("command %q was scheduled with different input digest", e.CommandID)
		}
		p.CommandDigests[e.CommandID] = e.InputDigest
		p.ScheduledCommands[e.CommandID] = *e
	case *session.CommandCompleted:
		copy := *e
		copy.Output = cloneRef(e.Output)
		p.CompletedCommands[e.CommandID] = copy
		delete(p.ScheduledCommands, e.CommandID)
	case *session.InputQueued:
		p.observeRuntimeSequence(e.TurnID, "")
		if _, exists := p.pendingInput[e.InputID]; !exists {
			p.pendingOrder = append(p.pendingOrder, e.InputID)
		}
		p.pendingInput[e.InputID] = cloneInput(*e)
		if e.Text != "" || len(e.Attachments) > 0 {
			pending := messages.Message{Role: messages.RoleUser, Content: e.Text, CreatedAt: e.CreatedAt}
			pending.ID = e.MessageID
			if pending.ID == "" {
				pending.ID = protocol.InputMessageID(protocol.InputID(e.InputID))
			}
			pending.ImagesFrozen = e.ImagesFrozen
			for _, ref := range e.Attachments {
				pending.ImageAttachments = append(pending.ImageAttachments, messages.ImageAttachment{MediaType: ref.MediaType, BlobHash: ref.Hash, BlobSize: ref.Size})
			}
			p.pendingByID[e.InputID] = pending
		}
	case *session.InputDelivered:
		p.observeRuntimeSequence(e.TurnID, "")
		if message, ok := p.pendingByID[e.InputID]; ok {
			p.History = append(p.History, message)
			p.historyStep = append(p.historyStep, "")
			delete(p.pendingByID, e.InputID)
		}
		delete(p.pendingInput, e.InputID)
		p.removePendingID(e.InputID)
	case *session.InputCancelled:
		delete(p.pendingByID, e.InputID)
		delete(p.pendingInput, e.InputID)
		p.removePendingID(e.InputID)
	case *session.AssistantCommitted:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
		message, err := session.DecodeMessage(e.Message)
		if err != nil {
			return err
		}
		p.History = append(p.History, message)
		p.historyStep = append(p.historyStep, e.StepID)
	case *session.ToolStarted:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
		key := makeToolFactKey(e.TurnID, e.StepID, e.Call.CallID)
		if _, exists := p.startedTools[key]; !exists {
			p.startedOrder = append(p.startedOrder, key)
		}
		p.startedTools[key] = cloneStarted(*e)
	case *session.ToolFinished:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
		key := p.resolveToolFactKey(e.TurnID, e.StepID, e.CallID)
		if _, exists := p.finishedTools[key]; !exists {
			p.finishedOrder = append(p.finishedOrder, key)
		}
		p.finishedTools[key] = cloneFinished(*e)
	case *session.ToolResultsProjected:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
		for _, result := range e.Results {
			key := p.resolveToolFactKey(e.TurnID, e.StepID, result.CallID)
			p.projected[key] = true
			content := result.Text
			if result.Status != "success" && result.Status != "ok" && !strings.HasPrefix(content, "Error: ") {
				content = "Error: " + content
			}
			toolMessage := messages.Message{Role: messages.RoleTool, ToolCallID: result.CallID, Content: content}
			toolMessage.ID = toolResultMessageID(key, result.Text, result.Status)
			p.History = append(p.History, toolMessage)
			p.historyStep = append(p.historyStep, e.StepID)
		}
	case *session.MemoryChanged:
		p.MemorySeen = true
		if e.Cleared {
			p.MemoryText = ""
		} else if e.Text != "" {
			p.MemoryText = e.Text
		}
	case *session.TasksChanged:
		p.Tasks = append([]session.Task(nil), e.Tasks...)
	case *session.GoalChanged:
		if e.Goal == nil || p.Goal == nil || p.Goal.ID != e.Goal.ID {
			p.GoalAccountingIDs = nil
		}
		p.Goal = protocol.CloneGoal(e.Goal)
		if e.Goal != nil && e.AccountingID != "" {
			if p.GoalAccountingIDs == nil {
				p.GoalAccountingIDs = make(map[string]bool)
			}
			p.GoalAccountingIDs[e.AccountingID] = true
		}
	case *session.WorkflowChanged:
		p.Workflow = e.Workflow
	case *session.ContextCompacted:
		message, err := session.DecodeMessage(e.Summary)
		if err != nil {
			return err
		}
		p.History = []messages.Message{message}
		p.historyStep = []string{""}
	case *session.ConversationReset:
		p.History = nil
		p.historyStep = nil
		p.startedTools = make(map[toolFactKey]session.ToolStarted)
		p.finishedTools = make(map[toolFactKey]session.ToolFinished)
		p.projected = make(map[toolFactKey]bool)
		p.startedOrder, p.finishedOrder = nil, nil
		for _, stored := range e.Messages {
			message, err := session.DecodeMessage(stored)
			if err != nil {
				return err
			}
			p.History = append(p.History, message)
			p.historyStep = append(p.historyStep, e.StepID)
		}
	case *session.UsageChanged:
		p.Usage = e.Usage
	case *session.AttemptFinished:
		delta := e.Attempt.Usage
		delta.TurnCount = 1 // one completed attempt, including older facts without TurnCount
		usage, err := session.AddUsage(p.Usage, delta)
		if err != nil {
			return err
		}
		p.Usage = usage
		p.observeRuntimeSequence(e.Attempt.TurnID, e.Attempt.StepID)
	case *session.TurnStarted:
		p.observeRuntimeSequence(e.TurnID, e.StepID)
	case *session.TurnFinished:
		p.observeRuntimeSequence(e.TurnID, "")
	case *session.RequestPrepared:
		p.observeRuntimeSequence(e.Manifest.TurnID, e.Manifest.StepID)
	case *session.ApprovalRequested, *session.ApprovalResolved, *session.HookStarted, *session.HookFinished, *session.ConversationRewound:
		// These facts do not currently have fields in SessionSnapshot.  They
		// remain available to richer projections and are intentionally not
		// guessed into history.
	case *session.ToolsDiscovered:
		if e.CatalogVersion > p.CatalogVersion {
			p.CatalogVersion = e.CatalogVersion
		}
		for _, tool := range e.Tools {
			p.Discovered[tool.ToolID] = cloneSchema(tool)
		}
	}

	return nil
}

func (p *State) removePendingID(id string) {
	for i, current := range p.pendingOrder {
		if current == id {
			copy(p.pendingOrder[i:], p.pendingOrder[i+1:])
			p.pendingOrder = p.pendingOrder[:len(p.pendingOrder)-1]
			return
		}
	}
}
