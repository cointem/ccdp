package protocol

import "testing"

func TestToolTerminal(t *testing.T) {
	for _, status := range []string{
		ToolStatusSuccess, ToolStatusError, ToolStatusDenied, ToolStatusCancelled,
		ToolStatusUnknown, ToolStatusResourceLimit, "done", "completed",
		"complete", "failed", "failure", "rejected", "canceled",
		"stopped", "interrupted", " UNKNOWN ",
	} {
		if !ToolTerminal(status) {
			t.Errorf("terminal status %q treated as active", status)
		}
	}
	for _, status := range []string{"", ToolStatusQueued, ToolStatusRunning, "pending", "starting", "awaiting_approval"} {
		if ToolTerminal(status) {
			t.Errorf("active status %q treated as terminal", status)
		}
	}
}
