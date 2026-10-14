package replay

import (
	"sort"

	"ccdp/internal/messages"
)

// orderedStartedIDs and orderedFinishedIDs retain deterministic call order
// even when ToolFinished facts arrive from parallel workers in completion
// order. CallIndex is the new explicit ordinal; insertion order is the
// compatibility fallback for old events that have no ordinal.
func (p *State) orderedStartedIDs() []toolFactKey {
	if p == nil {
		return nil
	}
	ids := append([]toolFactKey(nil), p.startedOrder...)
	seen := make(map[toolFactKey]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	for id := range p.startedTools {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		a, aok := p.startedTools[ids[i]]
		b, bok := p.startedTools[ids[j]]
		if aok && bok && ids[i].TurnID == ids[j].TurnID && ids[i].StepID != "" && ids[i].StepID == ids[j].StepID && a.CallIndex != b.CallIndex {
			return a.CallIndex < b.CallIndex
		}
		return false
	})
	return ids
}

func (p *State) orderedFinishedIDs() []toolFactKey {
	if p == nil {
		return nil
	}
	ids := append([]toolFactKey(nil), p.finishedOrder...)
	seen := make(map[toolFactKey]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	for id := range p.finishedTools {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	// Stable sort by the corresponding Started ordinal only within a step;
	// equal/legacy ordinals retain the raw completion order.
	sort.SliceStable(ids, func(i, j int) bool {
		a, aok := p.startedTools[ids[i]]
		b, bok := p.startedTools[ids[j]]
		if aok && bok && ids[i].TurnID == ids[j].TurnID && ids[i].StepID != "" && ids[i].StepID == ids[j].StepID && a.CallIndex != b.CallIndex {
			return a.CallIndex < b.CallIndex
		}
		return false
	})
	return ids
}

func (p *State) removeProjectedToolResult(key toolFactKey) {
	if p == nil || key.CallID == "" {
		return
	}
	filtered := p.History[:0]
	filteredSteps := p.historyStep[:0]
	for i, message := range p.History {
		stepID := ""
		if i < len(p.historyStep) {
			stepID = p.historyStep[i]
		}
		if message.Role == messages.RoleTool && message.ToolCallID == key.CallID && (key.StepID == "" || stepID == key.StepID) {
			continue
		}
		filtered = append(filtered, message)
		filteredSteps = append(filteredSteps, stepID)
	}
	p.History = filtered
	p.historyStep = filteredSteps
	delete(p.projected, key)
}

func (p *State) insertRecoveredToolResult(key toolFactKey, recovered messages.Message) {
	if p == nil || recovered.ToolCallID == "" {
		return
	}
	for i, assistant := range p.History {
		if assistant.Role != messages.RoleAssistant || !assistantHasCall(assistant, key.CallID) {
			continue
		}
		if key.StepID != "" && (i >= len(p.historyStep) || p.historyStep[i] != key.StepID) {
			continue
		}
		end := i + 1
		existing := make(map[string]messages.Message)
		existingSteps := make(map[string]string)
		for end < len(p.History) && p.History[end].Role == messages.RoleTool {
			result := p.History[end]
			if _, seen := existing[result.ToolCallID]; !seen {
				existing[result.ToolCallID] = result
				if end < len(p.historyStep) {
					existingSteps[result.ToolCallID] = p.historyStep[end]
				}
			}
			end++
		}
		if _, already := existing[key.CallID]; already {
			// A repeated call ID in a later step must be associated with the
			// next assistant block, not the already-complete earlier block.
			continue
		}
		ordered := make([]messages.Message, 0, len(assistant.ToolCalls))
		orderedSteps := make([]string, 0, len(assistant.ToolCalls))
		for _, call := range assistant.ToolCalls {
			if result, ok := existing[call.ID]; ok {
				ordered = append(ordered, result)
				orderedSteps = append(orderedSteps, existingSteps[call.ID])
			} else if call.ID == key.CallID {
				ordered = append(ordered, recovered)
				orderedSteps = append(orderedSteps, key.StepID)
			}
		}
		for offset, result := range p.History[i+1 : end] {
			if !assistantHasCall(assistant, result.ToolCallID) {
				ordered = append(ordered, result)
				stepID := ""
				if i+1+offset < len(p.historyStep) {
					stepID = p.historyStep[i+1+offset]
				}
				orderedSteps = append(orderedSteps, stepID)
			}
		}
		updated := make([]messages.Message, 0, len(p.History)+1)
		updatedSteps := make([]string, 0, len(p.History)+1)
		updated = append(updated, p.History[:i+1]...)
		if i+1 <= len(p.historyStep) {
			updatedSteps = append(updatedSteps, p.historyStep[:i+1]...)
		} else {
			for n := 0; n < i+1; n++ {
				updatedSteps = append(updatedSteps, "")
			}
		}
		updated = append(updated, ordered...)
		updatedSteps = append(updatedSteps, orderedSteps...)
		updated = append(updated, p.History[end:]...)
		if end < len(p.historyStep) {
			updatedSteps = append(updatedSteps, p.historyStep[end:]...)
		} else {
			for n := end; n < len(p.History); n++ {
				updatedSteps = append(updatedSteps, "")
			}
		}
		p.History = updated
		p.historyStep = updatedSteps
		return
	}
	// If the assistant fact is from a legacy projection that omitted the call
	// payload, keep the recovery fact visible rather than silently dropping it.
	p.History = append(p.History, recovered)
	p.historyStep = append(p.historyStep, key.StepID)
}

func assistantHasCall(message messages.Message, callID string) bool {
	for _, call := range message.ToolCalls {
		if call.ID == callID {
			return true
		}
	}
	return false
}

// toolResultMessageID is occurrence-scoped.  A provider is allowed to reuse a
// call ID in a later step, and equal output is not evidence that the two
// messages are the same conversation entry.  Keep the old content-only shape
// for legacy facts which have no occurrence identity; all new projections have
// a turn/step key and therefore cannot collide in the UI or replay cache.
func toolResultMessageID(key toolFactKey, text, status string) string {
	if key.TurnID == "" && key.StepID == "" {
		return messages.StableID("tool-result", struct {
			CallID string
			Text   string
			Status string
		}{CallID: key.CallID, Text: text, Status: status})
	}
	return messages.StableID("tool-result", struct {
		TurnID string
		StepID string
		CallID string
		Text   string
		Status string
	}{TurnID: key.TurnID, StepID: key.StepID, CallID: key.CallID, Text: text, Status: status})
}
