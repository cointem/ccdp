package messages

import (
	"encoding/json"

	"ccdp/internal/jsonvalue"
)

// Clone returns an independently mutable copy of the messages.
func Clone(src []Message) []Message {
	if src == nil {
		return nil
	}
	dst := make([]Message, len(src))
	for i, m := range src {
		dst[i] = m
		dst[i].ProviderContent = CloneProviderBlocks(m.ProviderContent)
		dst[i].StreamSegments = append([]StreamSegment(nil), m.StreamSegments...)
		dst[i].ToolCalls = append([]ToolCall(nil), m.ToolCalls...)
		dst[i].Content = m.Content
		if m.ImageAttachments != nil {
			dst[i].ImageAttachments = make([]ImageAttachment, len(m.ImageAttachments))
			for j, attachment := range m.ImageAttachments {
				dst[i].ImageAttachments[j] = attachment
				dst[i].ImageAttachments[j].Data = append([]byte(nil), attachment.Data...)
			}
		}
		for j := range dst[i].ToolCalls {
			dst[i].ToolCalls[j].Arguments = jsonvalue.Clone(m.ToolCalls[j].Arguments)
		}
	}
	return dst
}

func CloneProviderBlocks(src []json.RawMessage) []json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make([]json.RawMessage, len(src))
	for i := range src {
		dst[i] = append(json.RawMessage(nil), src[i]...)
	}
	return dst
}
