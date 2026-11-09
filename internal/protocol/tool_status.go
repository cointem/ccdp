package protocol

import "strings"

// ToolFailure records what the runtime actually knows, without inferring
// sandbox denial from command stderr or an ordinary nonzero exit status.
type ToolFailure struct {
	Code             string              `json:"code"`
	DecisionSource   string              `json:"decision_source,omitempty"`
	ExecutionStarted string              `json:"execution_started"` // yes, no, unknown
	RetryClass       string              `json:"retry_class"`
	SideEffect       string              `json:"side_effect,omitempty"` // none, completed, unknown; empty means unspecified
	Resources        []CapabilityRequest `json:"resources,omitempty"`
	DecisionKey      string              `json:"-"`
}

// ExecutionPhase describes progress, independently from a tool's terminal
// status and from the lifetime of a returned background-process handle.
type ExecutionPhase string

const (
	EventToolPhase         EventKind      = "tool_phase"
	ExecutionQueued        ExecutionPhase = "queued"
	ExecutionPreparing     ExecutionPhase = "preparing"
	ExecutionWaitingAccess ExecutionPhase = "waiting_for_access"
	ExecutionAdmitted      ExecutionPhase = "admitted"
	ExecutionDispatching   ExecutionPhase = "dispatching"
	ExecutionCompleted     ExecutionPhase = "completed"
)

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
