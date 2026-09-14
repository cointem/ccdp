package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"unicode/utf8"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type outputReader interface {
	Read(session.BlobRef, int64) ([]byte, error)
}

func (s *SessionSupervisor) readSessionRecords(ctx context.Context, id protocol.SessionID) ([]session.Record, outputReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var p *sessionPersistence
	if id == protocol.SessionID(s.root.sessionID) {
		p = s.root.persistenceHandle()
	} else {
		r, err := s.lookup(id)
		if err != nil {
			return nil, nil, err
		}
		r.mu.Lock()
		id = r.fact.Child.SessionID
		if r.agent != nil {
			p = r.agent.persistenceHandle()
		}
		if p == nil && r.memory != nil {
			records, blobs := append([]session.Record(nil), r.memory...), r.memoryBlobs
			r.mu.Unlock()
			return records, blobs, nil
		}
		r.mu.Unlock()
	}
	if p != nil {
		// Keep the writer/reader handle alive for this read. Closing the owner
		// may detach it, but cannot invalidate the copied immutable records.
		p.mu.Lock()
		var blobs outputReader = p.artifacts
		if p.memoryArtifacts != nil {
			blobs = p.memoryArtifacts
		}
		var records []session.Record
		var err error
		if p.store != nil {
			records, err = p.store.Read(session.Beginning)
		} else {
			err = session.ErrClosed
		}
		p.mu.Unlock()
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
			for _, block := range e.Message.Content {
				if result := block.ToolResult; result != nil && toolTranscriptID(e.TurnID, e.StepID, result.CallID) == itemID && !found {
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
		case *session.ToolFinished:
			if toolTranscriptID(e.TurnID, e.StepID, e.CallID) == itemID {
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
