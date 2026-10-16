package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"ccdp/internal/messages"
)

// EncodeMessage converts a message to durable content without allocating IDs
// or performing artifact I/O. Callers freeze attachment identities first.
func EncodeMessage(message messages.Message) (Message, error) {
	result := Message{ToolUpdate: messages.CloneToolUpdate(message.ToolUpdate), ProviderOrigin: message.ProviderOrigin, ProviderWire: message.ProviderWire, ProviderContent: messages.CloneProviderBlocks(message.ProviderContent), InternalSource: message.InternalSource, StreamSegments: append([]messages.StreamSegment(nil), message.StreamSegments...), ReasoningContent: message.ReasoningContent, MessageID: message.ID, Role: string(message.Role), CreatedAt: message.CreatedAt}
	result.ImagesFrozen = message.ImagesFrozen
	if result.Role == "" {
		result.Role = string(messages.RoleAssistant)
	}
	if message.Content != "" || len(message.ToolCalls) == 0 {
		result.Content = append(result.Content, ContentBlock{Kind: ContentText, Text: message.Content})
	}
	for _, attachment := range message.ImageAttachments {
		if attachment.BlobHash == "" {
			// The durable adapter fills this branch before calling the pure
			// converter. Legacy import has no attachment bytes to persist and
			// therefore leaves the original markdown text intact.
			continue
		}
		result.Content = append(result.Content, ContentBlock{Kind: ContentImage, Path: attachment.Path, Blob: &BlobRef{
			Hash: attachment.BlobHash, Size: attachment.BlobSize, MediaType: attachment.MediaType,
		}})
	}
	for _, call := range message.ToolCalls {
		args, err := json.Marshal(call.Arguments)
		if err != nil {
			return Message{}, fmt.Errorf("encode tool call %s: %w", call.ID, err)
		}
		result.Content = append(result.Content, ContentBlock{Kind: ContentToolCall, ToolCall: &ToolCall{
			CallID: call.ID, ToolID: call.Name, Arguments: args,
		}})
	}
	if message.Role == messages.RoleTool {
		// A tool result is represented as a typed result block, not free-form
		// assistant text.  The legacy message model carries only an error prefix.
		status := "success"
		text := message.Content
		if strings.HasPrefix(text, "Error: ") {
			status = "error"
			text = strings.TrimPrefix(text, "Error: ")
		}
		result.Content = []ContentBlock{{Kind: ContentToolResult, ToolResult: &ToolResult{
			CallID: message.ToolCallID, Status: status, Text: text, CreatedAt: message.CreatedAt,
		}}}
	}
	if len(result.Content) == 0 {
		result.Content = []ContentBlock{{Kind: ContentText}}
	}
	return result, nil
}

func DecodeMessage(message Message) (messages.Message, error) {
	result := messages.Message{ToolUpdate: messages.CloneToolUpdate(message.ToolUpdate), ProviderOrigin: message.ProviderOrigin, ProviderWire: message.ProviderWire, ProviderContent: messages.CloneProviderBlocks(message.ProviderContent), InternalSource: message.InternalSource, StreamSegments: append([]messages.StreamSegment(nil), message.StreamSegments...), ReasoningContent: message.ReasoningContent, ID: message.MessageID, Role: messages.Role(message.Role), CreatedAt: message.CreatedAt}
	result.ImagesFrozen = message.ImagesFrozen
	if result.Role == "" {
		return messages.Message{}, errors.New("agent: replayed message has no role")
	}
	var text strings.Builder
	for _, block := range message.Content {
		switch block.Kind {
		case ContentText:
			text.WriteString(block.Text)
		case ContentToolCall:
			if block.ToolCall == nil {
				return messages.Message{}, errors.New("agent: replayed tool call is missing payload")
			}
			var args map[string]any
			decoder := json.NewDecoder(strings.NewReader(string(block.ToolCall.Arguments)))
			decoder.UseNumber()
			if err := decoder.Decode(&args); err != nil || args == nil {
				if err == nil {
					err = errors.New("arguments must be a JSON object")
				}
				return messages.Message{}, fmt.Errorf("agent: replay tool call args: %w", err)
			}
			var trailing any
			if err := decoder.Decode(&trailing); err != io.EOF {
				if err == nil {
					return messages.Message{}, errors.New("agent: replay tool call args contain trailing JSON")
				}
				return messages.Message{}, fmt.Errorf("agent: replay tool call args: %w", err)
			}
			result.ToolCalls = append(result.ToolCalls, messages.ToolCall{ID: block.ToolCall.CallID, Name: block.ToolCall.ToolID, Arguments: args})
		case ContentToolResult:
			if block.ToolResult == nil {
				return messages.Message{}, errors.New("agent: replayed tool result is missing payload")
			}
			result.ToolCallID = block.ToolResult.CallID
			result.Content = block.ToolResult.Text
			if block.ToolResult.Status != "success" && block.ToolResult.Status != "ok" && !strings.HasPrefix(result.Content, "Error: ") {
				result.Content = "Error: " + result.Content
			}
			result.CreatedAt = block.ToolResult.CreatedAt
		case ContentImage:
			if block.Blob == nil {
				return messages.Message{}, errors.New("agent: replayed image is missing blob")
			}
			result.ImagesFrozen = true
			result.ImageAttachments = append(result.ImageAttachments, messages.ImageAttachment{Path: block.Path, MediaType: block.Blob.MediaType, BlobHash: block.Blob.Hash, BlobSize: block.Blob.Size})
		default:
			return messages.Message{}, fmt.Errorf("agent: replayed message has unknown content kind %q", block.Kind)
		}
	}
	if result.Content == "" && text.Len() > 0 {
		result.Content = text.String()
	}
	return result, nil
}

func MessageToolCalls(message Message) []ToolCall {
	var calls []ToolCall
	for _, block := range message.Content {
		if block.Kind == ContentToolCall && block.ToolCall != nil {
			calls = append(calls, *block.ToolCall)
		}
	}
	return calls
}
