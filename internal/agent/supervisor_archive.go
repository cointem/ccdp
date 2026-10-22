package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	records, _, recoveredThrough, err := s.readSessionRecordsWithRecovery(ctx, id)
	if err != nil {
		return protocol.TranscriptPage{}, err
	}
	// Keep only the requested page's text, not an unbounded text projection.
	// The compact identity index preserves stable pagination when later facts
	// complete tools that started before this page.
	t := sessionview.NewTranscript(before, limit)
	if err := t.ReplayRecords(ctx, records, recoveredThrough); err != nil {
		return protocol.TranscriptPage{}, err
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
	records, blobs, _, err := s.readSessionRecordsWithRecovery(ctx, id)
	return records, blobs, err
}

func (s *SessionSupervisor) readSessionRecordsWithRecovery(ctx context.Context, id protocol.SessionID) ([]session.Record, outputReader, session.Cursor, error) {
	if id != protocol.SessionID(s.root.SessionID()) {
		r, err := s.lookup(id)
		if err != nil {
			return nil, nil, 0, err
		}
		r.mu.Lock()
		id = r.fact.Child.SessionID
		r.mu.Unlock()
	}
	return s.readRecordedSessionRecordsWithRecovery(ctx, id)
}

// readRecordedSessionRecords also serves accounting traversal of descendant
// IDs found in the journal, including older nested children outside the live
// directory. Public child readers validate directory membership above.
func (s *SessionSupervisor) readRecordedSessionRecords(ctx context.Context, id protocol.SessionID) ([]session.Record, outputReader, error) {
	records, blobs, _, err := s.readRecordedSessionRecordsWithRecovery(ctx, id)
	return records, blobs, err
}

func (s *SessionSupervisor) readRecordedSessionRecordsWithRecovery(ctx context.Context, id protocol.SessionID) ([]session.Record, outputReader, session.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, 0, err
	}
	var reader interface {
		ReadRunRecords(context.Context) ([]session.Record, journal.ArtifactReader, error)
	}
	if id == protocol.SessionID(s.root.SessionID()) {
		reader = s.root
	} else if r := s.directory.Current(id); r != nil {
		r.mu.Lock()
		reader = r.agent
		r.mu.Unlock()
	}
	if reader != nil {
		// Keep the writer/reader handle alive for this read. Closing the owner
		// may detach it, but cannot invalidate the copied immutable records.
		if live, ok := reader.(interface {
			ReadRunRecordsWithRecovery(context.Context) ([]session.Record, journal.ArtifactReader, session.Cursor, error)
		}); ok {
			records, blobs, recoveredThrough, err := live.ReadRunRecordsWithRecovery(ctx)
			if !errors.Is(err, session.ErrClosed) {
				return records, blobs, recoveredThrough, err
			}
		} else {
			// Alternate live runtime adapters have no recovered prefix unless
			// they explicitly expose a recovery boundary.
			records, blobs, err := reader.ReadRunRecords(ctx)
			if !errors.Is(err, session.ErrClosed) {
				return records, blobs, 0, err
			}
		}
	}
	// Continuations keep complete prefixes of the same session log. A newly
	// queued run may have no runtime yet, so select the newest retained prefix,
	// rather than concatenating overlapping histories or choosing an old run.
	var saved []session.Record
	var artifacts outputReader
	// Nested archived children need not be executable directory entries, but
	// their journals still belong to accounting traversal after a memory reopen.
	for _, child := range s.memoryChildren {
		if len(child.records) == 0 {
			continue
		}
		created, ok := child.records[0].Event.(*session.SessionCreated)
		if ok && created.SessionID == string(id) &&
			(len(saved) == 0 || child.records[len(child.records)-1].Seq > saved[len(saved)-1].Seq) {
			saved, artifacts = child.records, child.blobs
		}
	}
	for _, r := range s.directory.Runs() {
		r.mu.Lock()
		if r.fact.Child.SessionID == id && len(r.memory) > 0 &&
			(len(saved) == 0 || r.memory[len(r.memory)-1].Seq > saved[len(saved)-1].Seq) {
			saved, artifacts = r.memory, r.memoryBlobs
		}
		r.mu.Unlock()
	}
	if saved != nil {
		return append([]session.Record(nil), saved...), artifacts, saved[len(saved)-1].Seq, nil
	}
	if s.memory {
		return nil, nil, 0, os.ErrNotExist // a queued child may not have opened a writer
	}
	store, err := session.OpenJSONLReadOnly(s.sessionDir, string(id))
	if err != nil {
		return nil, nil, 0, err
	}
	defer store.Close()
	if err := ctx.Err(); err != nil {
		return nil, nil, 0, err
	}
	records, err := store.Read(session.Beginning)
	if ctx.Err() != nil {
		return nil, nil, 0, ctx.Err()
	}
	var recoveredThrough session.Cursor
	if len(records) > 0 {
		recoveredThrough = records[len(records)-1].Seq
	}
	return records, session.OpenArtifactStoreReadOnly(filepath.Join(s.sessionDir, string(id))), recoveredThrough, err
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
