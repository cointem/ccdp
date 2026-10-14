package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/session"
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

// MemoryText reads the authoritative typed session memory projection.
func (a *Agent) MemoryText() string {
	if p := a.persistenceHandle(); p != nil {
		if text, _, err := p.MemoryText(); err == nil {
			return strings.TrimSpace(text)
		}
	}
	return ""
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
func (a *Agent) recordMemory() error {
	if !a.services.cfg.MemoryEnabled() {
		return nil
	}
	a.mu.Lock()
	userMsg := a.turnMemory.turnUserMsg
	touched := append([]string(nil), a.turnMemory.turnTouched...)
	summary := a.turnMemory.turnSummary
	a.mu.Unlock()

	if userMsg == "" && len(touched) == 0 && summary == "" {
		return nil
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

	text := strings.Join(entries, "\n\n") + "\n"
	maxMemoryBytes := int(session.DefaultMaxTransactionBytes / 2)
	if len(text) > maxMemoryBytes {
		text = messages.TruncateUTF8(text, maxMemoryBytes)
	}
	p := a.persistenceHandle()
	if p == nil {
		return fmt.Errorf("agent: session persistence is unavailable")
	}
	if err := p.PersistMemory(text, false); err != nil {
		a.markPersistenceFailure(err)
		return err
	}
	return nil
}

// ClearMemory records the cleared value in the typed session log.
func (a *Agent) ClearMemory() error {
	if a.owner == nil {
		return a.clearMemoryOwned()
	}
	a.ensureRun()
	r := memoryClearRequested{result: make(chan error, 1)}
	select {
	case a.owner.control <- r:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-r.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}

// recordTouched tracks a file path modified this turn (deduplicated) so the
// AutoMem entry can summarize what changed.
func (a *Agent) recordTouched(path string) {
	a.postTurnNote(turnNoteRecorded{touched: path})
}

// memoryOneLine flattens newlines and caps a string for the memory log.
func memoryOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = messages.TruncateUTF8(s, max) + "…"
	}
	return s
}
