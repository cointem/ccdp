package agent

import (
	"encoding/json"
	"fmt"
	"os"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

// emit publishes a runtime event to the typed subscription and diagnostic trace.
func (a *Agent) emit(ev protocol.EventView) {
	ev = protocol.CloneEventView(ev)
	if ev.Kind == protocol.EventToolResult {
		a.mu.Lock()
		if a.publications.deferToolEvents && (ev.Tool == nil || ev.Tool.ParentCallID == "") {
			a.publications.deferredToolEvents = append(a.publications.deferredToolEvents, ev)
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
	}
	// Slow subscribers recover through Snapshot; they never block execution.
	a.publishEvent(ev)
}

// emitStatus is a convenience wrapper.
func (a *Agent) emitStatus(format string, args ...any) {
	a.emit(protocol.EventView{Kind: protocol.EventStatus, Text: fmt.Sprintf(format, args...)})
}

// logv prints a diagnostic line to stderr when --verbose is set.
func (a *Agent) logv(format string, args ...any) {
	if a.services.cfg.Verbose {
		fmt.Fprintf(os.Stderr, "ccdp: "+format+"\n", args...)
	}
}

func toolEvent(tc messages.ToolCall, status, output string) protocol.EventView {
	args, _ := json.Marshal(tc.Arguments)
	return protocol.EventView{
		Kind: protocol.EventToolResult,
		Tool: &protocol.ToolView{
			ParentCallID: protocol.CallID(tc.ParentCallID), ChildIndex: tc.ChildIndex,
			ID:     protocol.CallID(tc.ID),
			Name:   tc.Name,
			Args:   args,
			Status: status,
			Output: truncateResult(output, 2000),
		},
	}
}

func (a *Agent) emitToolPhase(tc messages.ToolCall, phase protocol.ExecutionPhase) {
	ev := toolEvent(tc, "", "")
	ev.Kind, ev.Tool.Phase = protocol.EventToolPhase, phase
	a.emit(ev)
}
