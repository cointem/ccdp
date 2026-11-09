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
	if err := ctx.Err(); err != nil {
		return protocol.OutputPage{}, err
	}
	p := a.persistenceHandle()
	if p == nil {
		return protocol.OutputPage{}, session.ErrClosed
	}
	ref, blobs, err := p.OutputArtifact(outputID)
	if err != nil {
		return protocol.OutputPage{}, err
	}
	if offset > ref.Size {
		return protocol.OutputPage{}, errors.New("offset is outside output")
	}
	if err := ctx.Err(); err != nil {
		return protocol.OutputPage{}, err
	}
	if blobs == nil {
		return protocol.OutputPage{}, errors.New("output artifact unavailable")
	}
	if limit <= 0 {
		limit = 16 << 10
	}
	limit = min(limit, 64<<10)
	// One lookahead byte identifies a split final rune; a tiny requested page
	// must still hold at least one complete UTF-8 character.
	data, err := blobs.ReadRange(ref, offset, max(limit+1, utf8.UTFMax))
	if err != nil {
		return protocol.OutputPage{}, err
	}
	page, err := outputPage(string(data), 0, limit)
	page.Next += offset
	page.Total = ref.Size
	page.More = page.Next < ref.Size
	return page, err
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
