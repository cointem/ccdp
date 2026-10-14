package replay

import (
	"maps"
	"slices"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// Clone freezes the complete projection, including unfinished work. Recovery
// and observers may change the copy without changing the live reducer.
func (p *State) Clone() *State {
	if p == nil {
		return nil
	}
	q := *p
	q.Settings = p.Settings.Clone()
	q.History = messages.Clone(p.History)
	q.historyStep = slices.Clone(p.historyStep)
	q.Goal = protocol.CloneGoal(p.Goal)
	q.GoalAccountingIDs = maps.Clone(p.GoalAccountingIDs)
	q.Tasks = slices.Clone(p.Tasks)
	q.pendingOrder = slices.Clone(p.pendingOrder)
	q.pendingByID = make(map[string]messages.Message, len(p.pendingByID))
	for k, v := range p.pendingByID {
		q.pendingByID[k] = messages.Clone([]messages.Message{v})[0]
	}
	q.pendingInput = make(map[string]session.InputQueued, len(p.pendingInput))
	for k, v := range p.pendingInput {
		q.pendingInput[k] = cloneInput(v)
	}
	q.startedTools = make(map[toolFactKey]session.ToolStarted, len(p.startedTools))
	for k, v := range p.startedTools {
		q.startedTools[k] = cloneStarted(v)
	}
	q.finishedTools = make(map[toolFactKey]session.ToolFinished, len(p.finishedTools))
	for k, v := range p.finishedTools {
		q.finishedTools[k] = cloneFinished(v)
	}
	q.startedOrder = slices.Clone(p.startedOrder)
	q.finishedOrder = slices.Clone(p.finishedOrder)
	q.projected = maps.Clone(p.projected)
	q.CommandDigests = maps.Clone(p.CommandDigests)
	q.ScheduledCommands = maps.Clone(p.ScheduledCommands)
	q.CompletedCommands = make(map[string]session.CommandCompleted, len(p.CompletedCommands))
	for k, v := range p.CompletedCommands {
		v.Output = cloneRef(v.Output)
		q.CompletedCommands[k] = v
	}
	q.Discovered = make(map[string]session.ToolSchema, len(p.Discovered))
	for k, v := range p.Discovered {
		q.Discovered[k] = cloneSchema(v)
	}
	return &q
}

// Recovered adds synthetic crash outcomes only to a detached copy.
func (p *State) Recovered() *State { q := p.Clone(); q.FinalizeRecovery(); return q }

func cloneRef(p *session.BlobRef) *session.BlobRef {
	if p == nil {
		return nil
	}
	q := *p
	return &q
}

func cloneInput(v session.InputQueued) session.InputQueued {
	v.Blob = cloneRef(v.Blob)
	v.Attachments = slices.Clone(v.Attachments)
	return v
}

func cloneStarted(v session.ToolStarted) session.ToolStarted {
	v.Call.Arguments = slices.Clone(v.Call.Arguments)
	return v
}

func cloneFinished(v session.ToolFinished) session.ToolFinished {
	v.RawOutput = cloneRef(v.RawOutput)
	v.Result.Blob = cloneRef(v.Result.Blob)
	return v
}

func cloneSchema(v session.ToolSchema) session.ToolSchema {
	v.Schema = slices.Clone(v.Schema)
	v.Effects = slices.Clone(v.Effects)
	return v
}
