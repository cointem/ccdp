package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/messages"
)

// ContentKind and ContentBlock are the explicit message union used by model
// history.  Exactly one branch matching Kind is allowed.
type ContentKind string

const (
	ContentText       ContentKind = "text"
	ContentImage      ContentKind = "image"
	ContentToolCall   ContentKind = "tool_call"
	ContentToolResult ContentKind = "tool_result"
)

type ContentBlock struct {
	Kind ContentKind `json:"kind"`
	Text string      `json:"text,omitempty"`
	Blob *BlobRef    `json:"blob,omitempty"`
	// Path is an optional original locator for an image attachment. The blob,
	// not this path, is authoritative when rebuilding a request.
	Path       string      `json:"path,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

func (b ContentBlock) validate() error {
	switch b.Kind {
	case ContentText:
		if b.Blob != nil || b.Path != "" || b.ToolCall != nil || b.ToolResult != nil {
			return errors.New("text content block has non-text branches")
		}
	case ContentImage:
		if b.Blob == nil {
			return errors.New("image content block requires blob")
		}
		if err := b.Blob.validate(); err != nil {
			return err
		}
		if b.Text != "" || b.ToolCall != nil || b.ToolResult != nil {
			return errors.New("image content block has unrelated branches")
		}
	case ContentToolCall:
		if b.ToolCall == nil {
			return errors.New("tool-call content block requires tool_call")
		}
		if err := b.ToolCall.validate(); err != nil {
			return err
		}
		if b.Text != "" || b.Blob != nil || b.Path != "" || b.ToolResult != nil {
			return errors.New("tool-call content block has unrelated branches")
		}
	case ContentToolResult:
		if b.ToolResult == nil {
			return errors.New("tool-result content block requires tool_result")
		}
		if err := b.ToolResult.validate(); err != nil {
			return err
		}
		if b.Text != "" || b.Blob != nil || b.Path != "" || b.ToolCall != nil {
			return errors.New("tool-result content block has unrelated branches")
		}
	default:
		return fmt.Errorf("unknown content kind %q", b.Kind)
	}
	return nil
}

type Message struct {
	ToolUpdate       *messages.ToolUpdate     `json:"tool_update,omitempty"`
	ImagesFrozen     bool                     `json:"images_frozen,omitempty"`
	ProviderOrigin   string                   `json:"provider_origin,omitempty"`
	ProviderWire     string                   `json:"provider_wire,omitempty"`
	ProviderContent  []json.RawMessage        `json:"provider_content,omitempty"`
	InternalSource   string                   `json:"internal_source,omitempty"`
	StreamSegments   []messages.StreamSegment `json:"stream_segments,omitempty"`
	ReasoningContent string                   `json:"reasoning_content,omitempty"`
	MessageID        string                   `json:"message_id"`
	Role             string                   `json:"role"`
	Content          []ContentBlock           `json:"content"`
	// CreatedAt is the source timestamp for imported and newly committed
	// messages.  It is deliberately part of the typed message value instead
	// of being regenerated while replaying a log, so a projection can retain
	// legacy history timing exactly.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func (m Message) validate() error {
	if err := requireID("message_id", m.MessageID); err != nil {
		return err
	}
	if m.Role == "" {
		return errors.New("message role is required")
	}
	if len(m.Content) == 0 {
		return errors.New("message content is required")
	}
	for i, block := range m.Content {
		if err := block.validate(); err != nil {
			return fmt.Errorf("message content[%d]: %w", i, err)
		}
	}
	return nil
}

type Task struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

func (t Task) validate() error {
	if err := requireID("task_id", t.TaskID); err != nil {
		return err
	}
	if t.Title == "" || t.Status == "" {
		return errors.New("task title and status are required")
	}
	return nil
}

type AssistantCommitted struct {
	TurnID    string     `json:"turn_id"`
	StepID    string     `json:"step_id,omitempty"`
	Message   Message    `json:"message"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type ContextCompacted struct {
	BaseSeq        uint64   `json:"base_seq"`
	Summary        Message  `json:"summary"`
	KeptMessageIDs []string `json:"kept_message_ids"`
	SourceRevision uint64   `json:"source_revision"`
}

type ConversationReset struct {
	ResetID  string    `json:"reset_id"`
	Reason   string    `json:"reason,omitempty"`
	TurnID   string    `json:"turn_id,omitempty"`
	StepID   string    `json:"step_id,omitempty"`
	Messages []Message `json:"messages"`
}

type ConversationRewound struct {
	RewindID string `json:"rewind_id"`
	ToSeq    uint64 `json:"to_seq"`
	Reason   string `json:"reason,omitempty"`
}

type TasksChanged struct {
	Revision uint64 `json:"revision"`
	Tasks    []Task `json:"tasks"`
}

type MemoryChanged struct {
	Revision uint64   `json:"revision"`
	Text     string   `json:"text,omitempty"`
	Blob     *BlobRef `json:"blob,omitempty"`
	Cleared  bool     `json:"cleared,omitempty"`
}

func (e AssistantCommitted) Type() EventType { return EventTypeAssistantCommitted }

func (e AssistantCommitted) seal() {}

func (e ContextCompacted) Type() EventType { return EventTypeContextCompacted }

func (e ContextCompacted) seal() {}

func (e ConversationReset) Type() EventType { return EventTypeConversationReset }

func (e ConversationReset) seal() {}

func (e ConversationRewound) Type() EventType { return EventTypeConversationRewound }

func (e ConversationRewound) seal() {}

func (e TasksChanged) Type() EventType { return EventTypeTasksChanged }

func (e TasksChanged) seal() {}

func (e MemoryChanged) Type() EventType { return EventTypeMemoryChanged }

func (e MemoryChanged) seal() {}

func (e AssistantCommitted) validate() error {
	if err := requireID("turn_id", e.TurnID); err != nil {
		return err
	}
	if err := e.Message.validate(); err != nil {
		return err
	}
	for i, call := range e.ToolCalls {
		if err := call.validate(); err != nil {
			return fmt.Errorf("tool_calls[%d]: %w", i, err)
		}
	}
	return nil
}

func (e ContextCompacted) validate() error {
	if err := e.Summary.validate(); err != nil {
		return err
	}
	if len(e.KeptMessageIDs) == 0 {
		return errors.New("kept_message_ids are required")
	}
	for _, id := range e.KeptMessageIDs {
		if err := requireID("kept_message_id", id); err != nil {
			return err
		}
	}
	return nil
}

func (e ConversationReset) validate() error {
	if err := requireID("reset_id", e.ResetID); err != nil {
		return err
	}
	for _, message := range e.Messages {
		if err := message.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (e ConversationRewound) validate() error {
	return requireID("rewind_id", e.RewindID)
}

func (e TasksChanged) validate() error {
	for i, task := range e.Tasks {
		if err := task.validate(); err != nil {
			return fmt.Errorf("tasks[%d]: %w", i, err)
		}
	}
	return nil
}

func (e MemoryChanged) validate() error {
	if e.Text == "" && e.Blob == nil && !e.Cleared {
		return errors.New("memory requires text or blob")
	}
	if e.Blob != nil {
		return e.Blob.validate()
	}
	return nil
}
