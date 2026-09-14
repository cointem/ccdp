package agent

import (
	"context"
	"fmt"

	"ccdp/internal/messages"
)

// resetOutputBudget clears the per-turn aggregate budget.
func (a *Agent) resetOutputBudget() {
	a.mu.Lock()
	a.outputBudget = 0
	a.mu.Unlock()
}

// maybePersistResult writes very large tool results to disk and replaces them
// with a preview + path, so the model can re-read them (Claude Code's
// toolResultStorage). Small results pass through truncated as before.
func (a *Agent) maybePersistResult(tc messages.ToolCall, out string) string {
	a.mu.Lock()
	maxChars := a.cfg.MaxResultSizeChars
	a.mu.Unlock()
	if maxChars <= 0 {
		maxChars = toolJournalMaxText
	}
	if len(out) <= maxChars {
		return truncateResult(out, maxChars)
	}
	// The typed journal is the sole owner of complete tool output. This
	// compatibility path uses the same scoped artifact store and therefore
	// cannot create a session-shared outputs directory in memory or on disk.
	p := a.persistenceHandle()
	if p == nil {
		return truncateResult(out, maxChars)
	}
	ref, err := p.putBlob(context.Background(), []byte(out), "text/plain; charset=utf-8")
	if err != nil || ref == nil {
		return truncateResult(out, maxChars)
	}
	preview := truncateResult(out, maxChars/2)
	return fmt.Sprintf("%s\n\n[Full output retained in session artifact %s (%d bytes)]",
		preview, ref.Hash, ref.Size)
}

// clipAggregate enforces the per-turn aggregate tool-result budget: once the
// combined delivered characters exceed the cap, remaining results are clipped
// to a short preview.
func (a *Agent) clipAggregate(out string) string {
	a.mu.Lock()
	cap := a.cfg.MaxToolOutputCharsPerTurn
	a.mu.Unlock()
	if cap <= 0 {
		return out
	}
	a.mu.Lock()
	used := a.outputBudget
	remaining := cap - used
	if remaining > 0 {
		clipped := truncateResult(out, remaining)
		a.outputBudget += len(clipped)
		a.mu.Unlock()
		return clipped
	}
	a.mu.Unlock()
	return "…[aggregate tool output budget exhausted; result omitted]"
}
