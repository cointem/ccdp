package agent

import (
	"fmt"
	"os"
	"path/filepath"

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
	if len(out) <= a.cfg.MaxResultSizeChars {
		return truncateResult(out, a.cfg.MaxResultSizeChars)
	}
	dir := filepath.Join(a.cfg.SessionDir, "outputs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return truncateResult(out, a.cfg.MaxResultSizeChars)
	}
	path := filepath.Join(dir, tc.ID+".txt")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return truncateResult(out, a.cfg.MaxResultSizeChars)
	}
	preview := truncateResult(out, a.cfg.MaxResultSizeChars/2)
	return fmt.Sprintf("%s\n\n[Full output: %d chars saved to %s — read it with Read if you need the rest]",
		preview, len(out), path)
}

// clipAggregate enforces the per-turn aggregate tool-result budget: once the
// combined delivered characters exceed the cap, remaining results are clipped
// to a short preview.
func (a *Agent) clipAggregate(out string) string {
	cap := a.cfg.MaxToolOutputCharsPerTurn
	if cap <= 0 {
		return out
	}
	a.mu.Lock()
	used := a.outputBudget
	a.outputBudget += len(out)
	a.mu.Unlock()
	if used >= cap {
		head := out
		if len(head) > 2000 {
			head = head[:2000]
		}
		return head + "\n…[aggregate tool output budget exceeded; result clipped]"
	}
	return out
}
