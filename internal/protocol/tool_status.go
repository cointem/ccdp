package protocol

import "strings"

const (
	ToolStatusQueued        = "queued"
	ToolStatusRunning       = "running"
	ToolStatusSuccess       = "success"
	ToolStatusError         = "error"
	ToolStatusDenied        = "denied"
	ToolStatusCancelled     = "cancelled"
	ToolStatusUnknown       = "unknown"
	ToolStatusResourceLimit = "resource_limit"
)

// ToolTerminal reports whether an invocation has stopped producing a result.
// An unknown outcome is terminal, but does not imply success or safe retry.
// Older presentation aliases remain readable without changing stored statuses.
func ToolTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case ToolStatusSuccess, "done", "completed", "complete",
		ToolStatusError, "failed", "failure", ToolStatusDenied, "rejected",
		ToolStatusCancelled, "canceled", "stopped", "interrupted",
		ToolStatusUnknown, ToolStatusResourceLimit:
		return true
	}
	return false
}
