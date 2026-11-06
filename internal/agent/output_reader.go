package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func renderToolOutput(message messages.Message) string {
	if !message.DisplayTruncated || message.OutputRef == nil || strings.Contains(message.Content, message.OutputRef.Hash) {
		return message.Content
	}
	return message.Content + fmt.Sprintf("\n[Output shortened. Original: %d bytes. ReadOutput output_id=%q offset=0 limit=16384]",
		message.OutputRef.Size, message.OutputRef.Hash)
}

// readOutput grants access only to tool output referenced by this session.
// Request bodies, images, and another session's blobs are not output IDs.
func (a *Agent) readOutput(ctx context.Context, outputID string, offset int64, limit int) (protocol.OutputPage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if outputID == "" {
		return protocol.OutputPage{}, errors.New("output_id is required; use the ID in a tool result")
	}
	if offset < 0 {
		return protocol.OutputPage{}, errors.New("negative output offset")
	}
	records, blobs, err := a.ReadRunRecords(ctx)
	if err != nil {
		return protocol.OutputPage{}, err
	}
	ref, ok := sessionOutputReference(records, outputID)
	if !ok {
		return protocol.OutputPage{}, fmt.Errorf("output %q is not referenced by the current session", outputID)
	}
	if err := ctx.Err(); err != nil {
		return protocol.OutputPage{}, err
	}
	if blobs == nil {
		return protocol.OutputPage{}, errors.New("output artifact unavailable")
	}
	data, err := blobs.Read(ref, session.DefaultMaxBlobBytes)
	if err != nil {
		return protocol.OutputPage{}, err
	}
	return outputPage(string(data), offset, limit)
}

func sessionOutputReference(records []session.Record, id string) (session.BlobRef, bool) {
	match := func(ref *session.BlobRef) (session.BlobRef, bool) {
		if ref != nil && ref.Hash == id {
			return *ref, true
		}
		return session.BlobRef{}, false
	}
	messageRef := func(message session.Message) (session.BlobRef, bool) {
		for _, ref := range message.SourceOutputs {
			if ref.Hash == id {
				return ref, true
			}
		}
		for _, block := range message.Content {
			if block.ToolResult != nil {
				if ref, ok := match(block.ToolResult.Blob); ok {
					return ref, true
				}
			}
		}
		return session.BlobRef{}, false
	}
	for _, record := range records {
		switch e := session.EventPointer(record.Event).(type) {
		case *session.ProcessFinished:
			if ref, ok := match(&e.Result); ok {
				return ref, true
			}
		case *session.ToolFinished:
			if ref, ok := match(e.RawOutput); ok {
				return ref, true
			}
			if ref, ok := match(e.Result.Blob); ok {
				return ref, true
			}
		case *session.AssistantCommitted:
			if ref, ok := messageRef(e.Message); ok {
				return ref, true
			}
		case *session.ConversationReset:
			for _, message := range e.Messages {
				if ref, ok := messageRef(message); ok {
					return ref, true
				}
			}
		case *session.ContextCompacted:
			if ref, ok := messageRef(e.Summary); ok {
				return ref, true
			}
		}
	}
	return session.BlobRef{}, false
}

func outputPage(text string, offset int64, limit int) (protocol.OutputPage, error) {
	if offset < 0 || offset > int64(len(text)) {
		return protocol.OutputPage{}, errors.New("offset is outside output")
	}
	if limit <= 0 {
		limit = 16 << 10
	}
	limit = min(limit, 64<<10)
	start := int(offset)
	if start < len(text) && !utf8.RuneStart(text[start]) {
		return protocol.OutputPage{}, errors.New("offset is not a UTF-8 boundary")
	}
	end := min(len(text), start+limit)
	for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == start && start < len(text) {
		_, size := utf8.DecodeRuneInString(text[start:])
		end += size
	}
	return protocol.OutputPage{Text: text[start:end], Next: int64(end), Total: int64(len(text)), More: end < len(text)}, nil
}
