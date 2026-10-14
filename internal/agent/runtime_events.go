package agent

import (
	"fmt"
	"os"

	"ccdp/internal/messages"
)

// emit publishes a runtime event to the typed subscription and diagnostic trace.
func (a *Agent) emit(ev Event) {
	if ev.Type == EventToolResult {
		a.mu.Lock()
		if a.publications.deferToolEvents {
			a.publications.deferredToolEvents = append(a.publications.deferredToolEvents, ev)
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
	}
	a.writeTrace(ev)
	// Slow subscribers recover through Snapshot; they never block execution.
	a.publishEvent(ev)
}

// emitStatus is a convenience wrapper.
func (a *Agent) emitStatus(format string, args ...any) {
	a.emit(Event{Type: EventStatus, Text: fmt.Sprintf(format, args...)})
}

// logv prints a diagnostic line to stderr when --verbose is set.
func (a *Agent) logv(format string, args ...any) {
	if a.services.cfg.Verbose {
		fmt.Fprintf(os.Stderr, "ccdp: "+format+"\n", args...)
	}
}

// prettyArgs renders tool arguments compactly for display.
func prettyArgs(args map[string]any) string {
	if cmd, ok := args["command"].(string); ok {
		return cmd
	}
	if p, ok := args["file_path"].(string); ok {
		return p
	}
	return fmt.Sprintf("%v", args)
}

func toolEvent(tc messages.ToolCall, status, output string) Event {
	return Event{
		Type: EventToolResult,
		Tool: &ToolEvent{
			ID:     tc.ID,
			Name:   tc.Name,
			Args:   tc.Arguments,
			Status: status,
			Output: truncateResult(output, 2000),
		},
	}
}
