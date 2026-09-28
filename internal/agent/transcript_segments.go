package agent

import (
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"fmt"
)

func validStreamSegments(segments []messages.StreamSegment, text, reasoning string) bool {
	answerBytes, reasoningBytes := 0, 0
	for _, segment := range segments {
		if segment.Bytes <= 0 {
			return false
		}
		switch segment.Kind {
		case "assistant":
			answerBytes += segment.Bytes
		case "thinking":
			reasoningBytes += segment.Bytes
		default:
			return false
		}
	}
	return len(segments) > 0 && answerBytes == len(text) && reasoningBytes == len(reasoning)
}

// Committed segments replace their live IDs in place. The same durable
// lengths reconstruct the same ordering after reconnect/restart.
func (t *transcriptState) commitSegments(e session.AssistantCommitted, text string) bool {
	items := committedSegmentItems(e, text)
	if len(items) == 0 {
		return false
	}
	for i, item := range items {
		if i < len(t.previewIDs) {
			item.PreviousID = t.previewIDs[i]
		}
		t.put(item)
	}
	t.previewIDs = nil
	t.reasoningID, t.liveID, t.closedReasoningID = "", "", ""
	return true
}

// The transcript preview and the full-output reader must use the same IDs and
// segment boundaries. Only the preview applies a display truncation afterward.
func committedSegmentItems(e session.AssistantCommitted, text string) []protocol.TranscriptItem {
	if !validStreamSegments(e.Message.StreamSegments, text, e.Message.ReasoningContent) {
		return nil
	}
	items := make([]protocol.TranscriptItem, 0, len(e.Message.StreamSegments))
	answerOffset, reasoningOffset := 0, 0
	for i, segment := range e.Message.StreamSegments {
		var value string
		if segment.Kind == "assistant" {
			value = text[answerOffset : answerOffset+segment.Bytes]
			answerOffset += segment.Bytes
		} else {
			value = e.Message.ReasoningContent[reasoningOffset : reasoningOffset+segment.Bytes]
			reasoningOffset += segment.Bytes
		}
		items = append(items, protocol.TranscriptItem{ID: fmt.Sprintf("%s:segment:%d", e.Message.MessageID, i), Kind: segment.Kind, TurnID: protocol.TurnID(e.TurnID), Text: value, Status: "completed"})
	}
	return items
}

func (t *transcriptState) closePreview(id string) {
	for i := range t.items {
		if t.items[i].ID == id {
			t.items[i].Status = "completed"
			return
		}
	}
}
