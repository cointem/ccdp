package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// A cursor pins an immutable run or transcript item. It is not an execution ID
// the model must maintain to send messages or start the next task.
type agentReadCursor struct {
	Agent  protocol.SessionID `json:"agent"`
	View   string             `json:"view"`
	Run    protocol.RunID     `json:"run,omitempty"`
	Item   string             `json:"item,omitempty"`
	Offset int64              `json:"offset,omitempty"`
	Before uint64             `json:"before,omitempty"`
}

func encodeAgentCursor(c agentReadCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *SessionSupervisor) readAgent(ctx context.Context, req protocol.AgentReadRequest, budget int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := s.lookup(req.AgentID); err != nil {
		return "", err
	}
	if req.View == "" {
		req.View = "result"
	}
	c := agentReadCursor{Agent: req.AgentID, View: req.View, Item: req.ItemID}
	if req.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		if err != nil || json.Unmarshal(b, &c) != nil || c.Agent != req.AgentID || c.View != req.View || c.Offset < 0 {
			return "", errors.New("invalid agent read cursor")
		}
		if req.ItemID != "" && c.Item != req.ItemID {
			return "", errors.New("cursor belongs to another output item")
		}
	}
	page := protocol.AgentReadPage{AgentID: req.AgentID, View: req.View}
	switch req.View {
	case "result":
		if req.ItemID != "" {
			return "", errors.New("item_id is only valid with view=output")
		}
		run, err := s.savedAgentResult(req.AgentID, c.Run)
		if err != nil {
			return "", err
		}
		c.Run = run.ID
		page.Outcome = run.Status
		current, err := s.lookup(req.AgentID)
		if err != nil {
			return "", err
		}
		current.mu.Lock()
		page.PreviousTask = current.fact.Child.Run.ID != run.ID
		current.mu.Unlock()
		text := run.Output
		if run.Error != "" {
			text += "\nError: " + run.Error
		}
		if c.Offset > int64(len(text)) {
			return "", errors.New("result offset beyond end")
		}
		return encodeAgentTextPage(page, c, text[c.Offset:], false, budget)
	case "output":
		if c.Item == "" {
			return "", errors.New("view=output requires item_id on the first page")
		}
		out, err := s.ReadOutput(ctx, req.AgentID, c.Item, c.Offset, budget)
		if err != nil {
			return "", err
		}
		return encodeAgentTextPage(page, c, out.Text, out.More, budget)
	case "transcript":
		if req.ItemID != "" {
			return "", errors.New("item_id is only valid with view=output")
		}
		return s.readAgentTranscript(ctx, page, c, budget)
	default:
		return "", fmt.Errorf("unknown agent view %q", req.View)
	}
}

func (s *SessionSupervisor) savedAgentResult(id protocol.SessionID, runID protocol.RunID) (protocol.RunView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result protocol.RunView
	for _, r := range s.runs {
		r.mu.Lock()
		row := r.fact.Child
		saved := r.fact.UsageAccounted
		r.mu.Unlock()
		if row.SessionID != id || row.Run.Active() || row.Run.FinishedAt.IsZero() || !saved {
			continue
		}
		if runID != "" && row.Run.ID != runID {
			continue
		}
		if result.ID == "" || row.Run.FinishedAt.After(result.FinishedAt) {
			result = row.Run
		}
	}
	if result.ID == "" {
		return result, errors.New("no saved completed result; inspect ListAgents or wait for collaboration events")
	}
	return result, nil
}

// Measure the serialized page, not only its unescaped text. Binary search is
// bounded by the output size and keeps Unicode and continuation offsets intact.
func encodeAgentTextPage(page protocol.AgentReadPage, c agentReadCursor, text string, more bool, budget int) (string, error) {
	makePage := func(n int) []byte {
		for n > 0 && n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		page.Text = text[:n]
		page.More = more || n < len(text)
		page.NextCursor = ""
		if page.More {
			next := c
			next.Offset += int64(n)
			page.NextCursor = encodeAgentCursor(next)
		}
		b, _ := json.Marshal(page)
		return b
	}
	// The last page omits its cursor, so it can fit even when a slightly
	// shorter non-final page cannot. Check it before the monotonic search.
	if len(text) <= budget {
		if b := makePage(len(text)); len(b) <= budget {
			return string(b), nil
		}
	}
	lo, hi := 0, min(len(text), budget)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if len(makePage(mid)) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	b := makePage(lo)
	if len(b) > budget || (text != "" && page.Text == "") {
		return "", errors.New("output budget too small for agent page metadata")
	}
	return string(b), nil
}

func (s *SessionSupervisor) readAgentTranscript(ctx context.Context, page protocol.AgentReadPage, c agentReadCursor, budget int) (string, error) {
	records, _, err := s.readSessionRecords(ctx, c.Agent)
	if err != nil {
		return "", err
	}
	t := &transcriptState{index: make(map[string]uint64), before: c.Before, window: 64}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		t.facts([]session.Event{record.Event})
	}
	items, _ := t.snapshot()
	page.Items = []protocol.TranscriptItem{}
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		// History is an index with previews, not another copy of raw tool logs.
		if len(item.Text) > 2048 {
			item.Text = truncateUTF8Bytes(item.Text, 2048)
			item.Truncated = true
		}
		if len(item.Args) > 2048 {
			item.Args = nil
			item.Truncated = true
		}
		candidate := page
		candidate.Items = append([]protocol.TranscriptItem{item}, page.Items...)
		before := t.index[item.ID]
		candidate.More = before > 1
		candidate.NextCursor = ""
		if candidate.More {
			next := c
			next.Before = before
			candidate.NextCursor = encodeAgentCursor(next)
		}
		b, _ := json.Marshal(candidate)
		if len(b) > budget {
			if len(page.Items) > 0 {
				break
			}
			// Even the first oversized item must return an addressable ID.
			candidate.Items[0].Args = nil
			candidate.Items[0].Text = ""
			candidate.Items[0].Truncated = true
			b, _ = json.Marshal(candidate)
			if len(b) > budget {
				return "", errors.New("output budget too small for transcript item metadata")
			}
			page = candidate
			break
		}
		page = candidate
	}
	b, err := json.Marshal(page)
	return string(b), err
}
