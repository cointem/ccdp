package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
	"ccdp/internal/sessionview"
)

func (s *SessionSupervisor) ReadTranscript(ctx context.Context, id protocol.SessionID, before uint64, limit int) (protocol.TranscriptPage, error) {
	if limit <= 0 || limit > sessionview.Window {
		limit = sessionview.Window
	}
	records, _, err := s.readSessionRecords(ctx, id)
	if err != nil {
		return protocol.TranscriptPage{}, err
	}
	// Keep only the requested page's text, not an unbounded text projection.
	// The compact identity index preserves stable pagination when later facts
	// complete tools that started before this page.
	t := sessionview.NewTranscript(before, limit)
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return protocol.TranscriptPage{}, err
		}
		t.Facts([]session.Event{record.Event})
	}
	items, _ := t.Snapshot()
	var cursor uint64
	if len(items) > 0 {
		cursor = t.Position(items[0].ID)
	}
	return protocol.TranscriptPage{Items: items, Before: cursor, More: cursor > 1}, nil
}

type outputReader interface {
	Read(session.BlobRef, int64) ([]byte, error)
}

func (s *SessionSupervisor) readSessionRecords(ctx context.Context, id protocol.SessionID) ([]session.Record, outputReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var reader interface {
		ReadRunRecords(context.Context) ([]session.Record, journal.ArtifactReader, error)
	}
	if id == protocol.SessionID(s.root.SessionID()) {
		reader = s.root
	} else {
		r, err := s.lookup(id)
		if err != nil {
			return nil, nil, err
		}
		r.mu.Lock()
		id = r.fact.Child.SessionID
		if r.agent != nil {
			reader = r.agent
		}
		if reader == nil && r.memory != nil {
			records, blobs := append([]session.Record(nil), r.memory...), r.memoryBlobs
			r.mu.Unlock()
			return records, blobs, nil
		}
		r.mu.Unlock()
	}
	if reader != nil {
		// Keep the writer/reader handle alive for this read. Closing the owner
		// may detach it, but cannot invalidate the copied immutable records.
		records, blobs, err := reader.ReadRunRecords(ctx)
		if !errors.Is(err, session.ErrClosed) {
			return records, blobs, err
		}
	}
	store, err := session.OpenJSONLReadOnly(s.sessionDir, string(id))
	if err != nil {
		return nil, nil, err
	}
	defer store.Close()
	records, err := store.Read(session.Beginning)
	return records, session.OpenArtifactStoreReadOnly(filepath.Join(s.sessionDir, string(id))), err
}

func (s *SessionSupervisor) ReadOutput(ctx context.Context, id protocol.SessionID, itemID string, offset int64, limit int) (protocol.OutputPage, error) {
	if itemID == "" {
		return protocol.OutputPage{}, errors.New("item_id required; read session history first to select an item")
	}
	if offset < 0 {
		return protocol.OutputPage{}, errors.New("negative output offset")
	}
	if limit <= 0 || limit > 64<<10 {
		limit = 64 << 10
	}
	records, blobs, err := s.readSessionRecords(ctx, id)
	if err != nil {
		return protocol.OutputPage{}, err
	}
	var text string
	var ref *session.BlobRef
	found := false
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return protocol.OutputPage{}, err
		}
		switch e := record.Event.(type) {
		case *session.InputQueued:
			messageID := e.MessageID
			if messageID == "" {
				messageID = inputMessageID(e.InputID)
			}
			if messageID == itemID {
				text = e.Text
				found = true
			}
		case *session.AssistantCommitted:
			if value, blob, ok := committedOutput(e, itemID); ok {
				text, ref, found = value, blob, true
			}
		case *session.ConversationReset:
			for _, message := range e.Messages {
				committed := &session.AssistantCommitted{TurnID: e.TurnID, StepID: e.StepID, Message: message}
				if value, blob, ok := committedOutput(committed, itemID); ok {
					text, ref, found = value, blob, true
				}
			}
		case *session.ToolFinished:
			if sessionview.ToolID(e.TurnID, e.StepID, e.CallID) == itemID {
				text = e.Result.Text
				ref = e.RawOutput
				if ref == nil {
					ref = e.Result.Blob
				}
				found = true
			}
		case *session.TurnFinished:
			if "error:"+e.TurnID == itemID {
				text = e.Error
				found = true
			}
		}
	}
	if !found {
		return protocol.OutputPage{}, fmt.Errorf("saved transcript item %q not found", itemID)
	}
	if ref != nil {
		if blobs == nil {
			return protocol.OutputPage{}, errors.New("output artifact unavailable")
		}
		data, err := blobs.Read(*ref, 64<<20)
		if err != nil {
			return protocol.OutputPage{}, err
		}
		text = string(data)
	}
	if offset > int64(len(text)) {
		return protocol.OutputPage{}, errors.New("offset exceeds output length")
	}
	start := int(offset)
	if start < len(text) && !utf8.RuneStart(text[start]) {
		return protocol.OutputPage{}, errors.New("offset is not a UTF-8 boundary")
	}
	end := min(len(text), start+limit)
	for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == start && start < len(text) {
		_, n := utf8.DecodeRuneInString(text[start:])
		end = start + n
	}
	return protocol.OutputPage{Text: text[start:end], Next: int64(end), Total: int64(len(text)), More: end < len(text)}, nil
}

func committedOutput(e *session.AssistantCommitted, itemID string) (text string, ref *session.BlobRef, found bool) {
	if strings.HasPrefix(itemID, e.Message.MessageID+":segment:") {
		var answer strings.Builder
		for _, block := range e.Message.Content {
			if block.Kind == session.ContentText {
				answer.WriteString(block.Text)
			}
		}
		for _, item := range sessionview.CommittedSegments(*e, answer.String()) {
			if item.ID == itemID {
				text, found = item.Text, true
				break
			}
		}
	}
	if "reasoning:"+e.Message.MessageID == itemID && e.Message.ReasoningContent != "" {
		text = e.Message.ReasoningContent
		found = true
	}
	for _, block := range e.Message.Content {
		if result := block.ToolResult; result != nil && sessionview.ToolID(e.TurnID, e.StepID, result.CallID) == itemID && !found {
			text = result.Text
			ref = result.Blob
			found = true
		}
	}
	if e.Message.MessageID == itemID {
		found = true
		text = ""
		for _, block := range e.Message.Content {
			if block.Kind == session.ContentText {
				text += block.Text
			}
		}
	}
	return
}
