package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AutoMem is a lightweight session memory (Claude Code's AutoMem, simplified):
// after each turn the agent records the user request, the files it touched and
// the outcome into a per-session log, then injects that log into later system
// prompts so long-horizon facts survive context compaction and tool-result
// truncation. Enabled via config enable_memory.
const (
	// maxMemoryEntries bounds the log so the injected section stays small.
	maxMemoryEntries = 30
)

// memoryFile returns the AutoMem session memory file path (scoped per session
// id so it survives /resume).
func (a *Agent) memoryFile() string {
	return filepath.Join(a.cfg.SessionDir, a.sessionID+"-memory.md")
}

// MemoryText loads the session memory log (used by /memory).
func (a *Agent) MemoryText() string {
	data, err := os.ReadFile(a.memoryFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// MemorySection renders the memory log for injection into the system prompt.
// Empty memory yields an empty string so the prompt stays cache-stable.
func (a *Agent) MemorySection() string {
	text := a.MemoryText()
	if text == "" {
		return ""
	}
	return "\n\n# Session memory (facts learned earlier in this session)\n\n" + text
}

// recordMemory appends one AutoMem entry for the turn that just finished and
// caps the log at maxMemoryEntries. Called from turnFinished.
func (a *Agent) recordMemory() {
	if !a.cfg.MemoryEnabled() {
		return
	}
	a.mu.Lock()
	userMsg := a.turnUserMsg
	touched := append([]string(nil), a.turnTouched...)
	summary := a.turnSummary
	a.mu.Unlock()

	if userMsg == "" && len(touched) == 0 && summary == "" {
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "### %s\n", time.Now().Format("2006-01-02 15:04"))
	if userMsg != "" {
		fmt.Fprintf(&sb, "- Request: %s\n", memoryOneLine(userMsg, 160))
	}
	if len(touched) > 0 {
		sort.Strings(touched)
		fmt.Fprintf(&sb, "- Touched: %s\n", strings.Join(touched, ", "))
	}
	if summary != "" {
		fmt.Fprintf(&sb, "- Outcome: %s\n", memoryOneLine(summary, 240))
	}
	entry := strings.TrimRight(sb.String(), "\n")

	// Keep only the most recent entries (bounded memory). The existing log is
	// split back into individual entries before capping.
	entries := []string{entry}
	if prev := a.MemoryText(); prev != "" {
		entries = append(strings.Split(prev, "\n\n"), entries...)
	}
	if len(entries) > maxMemoryEntries {
		entries = entries[len(entries)-maxMemoryEntries:]
	}

	if err := os.MkdirAll(a.cfg.SessionDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(a.memoryFile(), []byte(strings.Join(entries, "\n\n")+"\n"), 0o600)
}

// ClearMemory empties the session memory log (/memory clear).
func (a *Agent) ClearMemory() error {
	if err := os.Remove(a.memoryFile()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// recordTouched tracks a file path modified this turn (deduplicated) so the
// AutoMem entry can summarize what changed.
func (a *Agent) recordTouched(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.turnTouched {
		if p == path {
			return
		}
	}
	a.turnTouched = append(a.turnTouched, path)
}

// memoryOneLine flattens newlines and caps a string for the memory log.
func memoryOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
