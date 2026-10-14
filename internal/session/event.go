package session

import (
	"fmt"
	"strings"
	"time"
)

const sha256HexLength = 64

// EventType is the stable name written in the event log.  EventType is not an
// open string bag: only the concrete event types declared in this package can
// satisfy Event, and the decoder rejects names it does not know.
type EventType string

const (
	EventTypeSessionCreated       EventType = "SessionCreated"
	EventTypeSessionImported      EventType = "SessionImported"
	EventTypeSessionClosed        EventType = "SessionClosed"
	EventTypeInputQueued          EventType = "InputQueued"
	EventTypeInputDelivered       EventType = "InputDelivered"
	EventTypeInputCancelled       EventType = "InputCancelled"
	EventTypeCommandScheduled     EventType = "CommandScheduled"
	EventTypeCommandCompleted     EventType = "CommandCompleted"
	EventTypeSettingsChanged      EventType = "SettingsChanged"
	EventTypeWorkflowChanged      EventType = "WorkflowChanged"
	EventTypeGoalChanged          EventType = "GoalChanged"
	EventTypeTurnStarted          EventType = "TurnStarted"
	EventTypeTurnFinished         EventType = "TurnFinished"
	EventTypeRequestPrepared      EventType = "RequestPrepared"
	EventTypeAttemptFinished      EventType = "AttemptFinished"
	EventTypeAssistantCommitted   EventType = "AssistantCommitted"
	EventTypeToolStarted          EventType = "ToolStarted"
	EventTypeToolFinished         EventType = "ToolFinished"
	EventTypeToolResultsProjected EventType = "ToolResultsProjected"
	EventTypeApprovalRequested    EventType = "ApprovalRequested"
	EventTypeApprovalResolved     EventType = "ApprovalResolved"
	EventTypeHookStarted          EventType = "HookStarted"
	EventTypeHookFinished         EventType = "HookFinished"
	EventTypeContextCompacted     EventType = "ContextCompacted"
	EventTypeConversationReset    EventType = "ConversationReset"
	EventTypeConversationRewound  EventType = "ConversationRewound"
	EventTypeTasksChanged         EventType = "TasksChanged"
	EventTypeMemoryChanged        EventType = "MemoryChanged"
	EventTypeToolsDiscovered      EventType = "ToolsDiscovered"
	EventTypeUsageChanged         EventType = "UsageChanged"
)

// Event is the sealed, typed fact interface used by Store.  External packages
// can construct the concrete values below but cannot add a payload type that
// the JSONL codec would silently accept.
type Event interface {
	Type() EventType
	validate() error
	seal()
}

// BlobRef points at an immutable content-addressed artifact in a session's
// blobs directory.  The store never puts blob bytes directly in an event.
type BlobRef struct {
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type,omitempty"`
}

func (b BlobRef) validate() error {
	if len(b.Hash) != 64 {
		return fmt.Errorf("blob hash must be a 64-character SHA-256 hex string")
	}
	for _, r := range b.Hash {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return fmt.Errorf("blob hash contains non-hex character")
		}
	}
	if b.Size < 0 {
		return fmt.Errorf("blob size must not be negative")
	}
	return nil
}

func requireID(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s contains a newline", name)
	}
	return nil
}

func isSHA256Digest(value string) bool {
	if len(value) != sha256HexLength {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func validateTime(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func validateOutcome(value string) error {
	switch value {
	case "success", "error", "cancelled", "interrupted", "unknown", "denied", "queued", "applied", "rejected":
		return nil
	default:
		return fmt.Errorf("unknown outcome %q", value)
	}
}
