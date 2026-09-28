package agent

import "ccdp/internal/messages"

func estimateHistoryTokens(history []messages.Message) int {
	total := 0
	for _, m := range history {
		total += estimateMessageTokens(m)
	}
	return total
}

// compactCut chooses a boundary between complete tool groups. A user turn may
// span many groups; preserving the whole turn would prevent useful compaction.
func compactCut(history []messages.Message, keep, budget int) int {
	suffix := make([]int, len(history)+1)
	for i := len(history) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + estimateMessageTokens(history[i])
	}
	pending := make(map[string]bool)
	boundaries := []int{0}
	for i, m := range history {
		for _, call := range m.ToolCalls {
			pending[call.ID] = true
		}
		if m.Role == messages.RoleTool {
			delete(pending, m.ToolCallID)
		}
		if len(pending) == 0 {
			boundaries = append(boundaries, i+1)
		}
	}
	preferred := max(0, len(history)-keep)
	cut := 0
	for _, boundary := range boundaries {
		if boundary <= preferred {
			cut = boundary
		}
	}
	// KeepAfterCompact is a preference, not permission to retain an unbounded
	// tail. Whole completed groups can move into the summary when necessary.
	for _, boundary := range boundaries {
		if boundary >= cut && suffix[cut] > budget {
			cut = boundary
		}
	}
	return cut
}
