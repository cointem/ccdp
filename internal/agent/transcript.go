package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

const transcriptWindow = 256
const transcriptTextLimit = 16 << 10

func toolTranscriptID(turn, step, call string) string {
	if step == "" {
		return "tool:" + turn + ":" + call
	}
	return "tool:" + turn + ":" + step + ":" + call
}

// The compatibility event protocol uses numeric turn/step IDs; durable
// journal IDs carry prefixes. Normalize only at this adapter boundary.
func toolEventTranscriptID(ev protocol.EventView) string {
	turn, step := string(ev.TurnID), string(ev.StepID)
	if _, err := strconv.ParseUint(turn, 10, 64); err == nil {
		turn = "turn-" + turn
	}
	if _, err := strconv.ParseUint(step, 10, 64); err == nil {
		step = "step-" + step
	}
	call := string(ev.CallID)
	if ev.Tool != nil {
		call = string(ev.Tool.ID)
	}
	return toolTranscriptID(turn, step, call)
}

// The projection is fed at commit/publication, not through a lossy Watch.
// Its mutex never calls back into Agent or persistence.
type transcriptState struct {
	index       map[string]uint64
	before      uint64
	window      int
	mu          sync.Mutex
	items       []protocol.TranscriptItem
	more        bool
	reasoningID string
	liveID      string
	serial      uint64
}

func clipTranscript(s string) (string, bool) {
	if len(s) <= transcriptTextLimit {
		return s, false
	}
	n := transcriptTextLimit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

func (t *transcriptState) put(item protocol.TranscriptItem) {
	if t.index != nil {
		ordinal := t.index[item.ID]
		if ordinal == 0 {
			ordinal = uint64(len(t.index)) + 1
			t.index[item.ID] = ordinal
		}
		if t.before > 0 && ordinal >= t.before {
			return
		}
		if len(t.items) > 0 && ordinal < t.index[t.items[0].ID] {
			return
		}
	}
	var clipped bool
	item.Text, clipped = clipTranscript(item.Text)
	item.Truncated = item.Truncated || clipped
	item.Args = append(json.RawMessage(nil), item.Args...)
	if len(item.Args) > transcriptTextLimit {
		item.Args = nil
		item.Truncated = true
	}
	for i := range t.items {
		if t.items[i].ID == item.ID {
			t.items[i] = item
			return
		}
	}
	t.items = append(t.items, item)
	window := t.window
	if window <= 0 {
		window = transcriptWindow
	}
	if len(t.items) > window {
		t.items = append([]protocol.TranscriptItem(nil), t.items[len(t.items)-window:]...)
		t.more = true
	}
}

func (t *transcriptState) snapshot() ([]protocol.TranscriptItem, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	items := append([]protocol.TranscriptItem(nil), t.items...)
	for i := range items {
		items[i].Args = append(json.RawMessage(nil), items[i].Args...)
	}
	return items, t.more
}

func (t *transcriptState) facts(events []session.Event) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, fact := range events {
		if !transcriptRelevantEvent(fact.Type()) {
			continue
		}
		// Normalize through the sealed codec to handle both live values and replay pointers.
		b, err := json.Marshal(fact)
		if err != nil {
			continue
		}
		switch fact.Type() {
		case session.EventTypeConversationReset:
			t.applyConversationReset()
		case session.EventTypeInputQueued:
			t.applyInputQueued(b)
		case session.EventTypeInputCancelled:
			t.applyInputCancelled(b)
		case session.EventTypeAssistantCommitted:
			t.applyAssistantCommitted(b)
		case session.EventTypeToolStarted:
			t.applyToolStarted(b)
		case session.EventTypeToolFinished:
			t.applyToolFinished(b)
		case session.EventTypeTurnFinished:
			t.applyTurnFinished(b)
		}
	}
}

// transcriptRelevantEvent reports whether an event type contributes to the
// transcript projection.
func transcriptRelevantEvent(typ session.EventType) bool {
	switch typ {
	case session.EventTypeInputQueued, session.EventTypeInputCancelled, session.EventTypeAssistantCommitted,
		session.EventTypeToolStarted, session.EventTypeToolFinished, session.EventTypeTurnFinished,
		session.EventTypeConversationReset:
		return true
	}
	return false
}

func (t *transcriptState) applyConversationReset() {
	if t.index == nil {
		t.items = nil
		t.more = false
		t.liveID = ""
	}
}

func (t *transcriptState) applyInputQueued(b []byte) {
	var e session.InputQueued
	if json.Unmarshal(b, &e) != nil {
		return
	}
	id := e.MessageID
	if id == "" {
		id = inputMessageID(e.InputID)
	}
	t.put(protocol.TranscriptItem{ID: id, Kind: "user", Text: e.Text, Status: "queued"})
}

func (t *transcriptState) applyInputCancelled(b []byte) {
	var e session.InputCancelled
	if json.Unmarshal(b, &e) != nil {
		return
	}
	for _, item := range t.items {
		if item.ID == inputMessageID(e.InputID) {
			item.Status = "cancelled"
			t.put(item)
			break
		}
	}
}

func (t *transcriptState) applyAssistantCommitted(b []byte) {
	var e session.AssistantCommitted
	if json.Unmarshal(b, &e) != nil {
		return
	}
	var text strings.Builder
	for _, block := range e.Message.Content {
		if block.Kind == session.ContentText {
			text.WriteString(block.Text)
		}
	}
	if e.Message.Role == "assistant" && e.Message.ReasoningContent != "" {
		id := "reasoning:" + e.Message.MessageID
		previous := t.reasoningID
		for i := range t.items {
			if previous != "" && t.items[i].ID == previous {
				t.items[i].ID = id
			}
		}
		t.put(protocol.TranscriptItem{ID: id, PreviousID: previous, Kind: "thinking", TurnID: protocol.TurnID(e.TurnID), Text: e.Message.ReasoningContent, Status: "completed"})
	}
	if e.Message.Role == "assistant" {
		for i := range t.items {
			if t.items[i].ID == t.reasoningID {
				t.items[i].Status = "completed"
			}
		}
		t.reasoningID = ""
	}
	previousID := ""
	if t.liveID != "" && e.Message.Role == "assistant" {
		previousID = t.liveID
		for i := range t.items {
			if t.items[i].ID == t.liveID {
				t.items[i].ID = e.Message.MessageID
			}
		}
		t.liveID = ""
	}
	if text.Len() > 0 {
		t.put(protocol.TranscriptItem{ID: e.Message.MessageID, PreviousID: previousID, Kind: e.Message.Role, TurnID: protocol.TurnID(e.TurnID), Text: text.String(), Status: "completed"})
	}
	for _, block := range e.Message.Content {
		if block.ToolCall != nil {
			call := block.ToolCall
			t.put(protocol.TranscriptItem{ID: toolTranscriptID(e.TurnID, e.StepID, call.CallID), Kind: "tool", TurnID: protocol.TurnID(e.TurnID), StepID: protocol.StepID(e.StepID), CallID: protocol.CallID(call.CallID), Tool: call.ToolID, Args: call.Arguments, Status: "queued"})
		}
		if block.ToolResult != nil {
			result := block.ToolResult
			id := toolTranscriptID(e.TurnID, e.StepID, result.CallID)
			item := protocol.TranscriptItem{ID: id, Kind: "tool", TurnID: protocol.TurnID(e.TurnID), CallID: protocol.CallID(result.CallID), Text: result.Text, Status: result.Status}
			found := false
			for _, old := range t.items {
				if old.ID == id {
					found = true
					if old.Status == "queued" {
						old.Text, old.Status = result.Text, result.Status
						old.Truncated = result.Blob != nil
						t.put(old)
					}
					break
				}
			}
			if !found {
				t.put(item)
			}
		}
	}
}

func (t *transcriptState) applyToolStarted(b []byte) {
	var e session.ToolStarted
	if json.Unmarshal(b, &e) != nil {
		return
	}
	t.put(protocol.TranscriptItem{ID: toolTranscriptID(e.TurnID, e.StepID, e.Call.CallID), Kind: "tool", TurnID: protocol.TurnID(e.TurnID), StepID: protocol.StepID(e.StepID), CallID: protocol.CallID(e.Call.CallID), Tool: e.Call.ToolID, Args: e.Call.Arguments, Status: "running"})
}

func (t *transcriptState) applyToolFinished(b []byte) {
	var e session.ToolFinished
	if json.Unmarshal(b, &e) != nil {
		return
	}
	id := toolTranscriptID(e.TurnID, e.StepID, e.CallID)
	item := protocol.TranscriptItem{ID: id, Kind: "tool", CallID: protocol.CallID(e.CallID), TurnID: protocol.TurnID(e.TurnID)}
	for _, old := range t.items {
		if old.ID == id {
			item = old
			break
		}
	}
	item.Text, item.Status = e.Result.Text, e.Status
	item.Truncated = e.RawOutput != nil && e.RawOutput.Size > int64(len(item.Text)) || e.Result.Blob != nil
	if item.Text == "" {
		item.Text = e.Error
	}
	t.put(item)
}

func (t *transcriptState) applyTurnFinished(b []byte) {
	var e session.TurnFinished
	if json.Unmarshal(b, &e) != nil {
		return
	}
	if t.liveID != "" {
		for i := range t.items {
			if t.items[i].ID == t.liveID {
				t.items[i].Status = "interrupted"
			}
		}
		t.liveID = ""
	}
	if e.Error != "" {
		t.put(protocol.TranscriptItem{ID: "error:" + e.TurnID, Kind: "error", Text: e.Error, TurnID: protocol.TurnID(e.TurnID), Status: e.Outcome})
	}
}

func (t *transcriptState) event(ev protocol.EventView) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if ev.Kind != protocol.EventReasoning && ev.Kind != protocol.EventStatus && ev.Kind != protocol.EventUsage && ev.Kind != protocol.EventStateChanged && t.reasoningID != "" {
		for i := range t.items {
			if t.items[i].ID == t.reasoningID {
				t.items[i].Status = "completed"
			}
		}
		// Reasoning and answer deltas may interleave within one completion.
		// Usage/status events are not a new reasoning segment either.
		switch ev.Kind {
		case protocol.EventToolStarted, protocol.EventTurnDone, protocol.EventUserMessage, protocol.EventError:
			t.reasoningID = ""
		}
	}
	switch ev.Kind {
	case protocol.EventReasoning:
		for _, old := range t.items {
			if old.ID == t.reasoningID && old.TurnID != ev.TurnID {
				t.reasoningID = ""
				break
			}
		}
		if t.reasoningID == "" {
			t.serial++
			t.reasoningID = fmt.Sprintf("reasoning:%s:%d", ev.TurnID, t.serial)
		}
		item := protocol.TranscriptItem{ID: t.reasoningID, Kind: "thinking", TurnID: ev.TurnID, Status: "streaming"}
		for _, old := range t.items {
			if old.ID == item.ID {
				item = old
				break
			}
		}
		if !item.Truncated {
			item.Text += ev.Text
		}
		t.put(item)
	case protocol.EventStream:
		if ev.Text == "" {
			// An empty text delta only closes a still-open reasoning cell (the
			// finalize above already ran for any non-reasoning event); it must
			// not open a blank assistant cell. Anthropic uses this to finish a
			// thinking block that is followed by a tool_use (no answer text).
			return
		}
		if t.liveID == "" {
			t.serial++
			t.liveID = fmt.Sprintf("stream:%s:%d", ev.TurnID, t.serial)
		}
		item := protocol.TranscriptItem{ID: t.liveID, Kind: "assistant", TurnID: ev.TurnID, Status: "streaming"}
		for _, old := range t.items {
			if old.ID == item.ID {
				item = old
				break
			}
		}
		if !item.Truncated {
			item.Text += ev.Text
		}
		t.put(item)
	case protocol.EventToolProgress:
		if ev.Tool == nil {
			return
		}
		id := toolEventTranscriptID(ev)
		for _, old := range t.items {
			if old.ID == id && old.Status == "running" {
				if !old.Truncated {
					old.Text += ev.Tool.Output
				}
				t.put(old)
				return
			}
		}
	}
}

// Attach an owned, cumulative preview to a live event. A subscriber opening
// halfway through a message can upsert by ID without duplicating the prefix.
func (t *transcriptState) eventItem(ev protocol.EventView) *protocol.TranscriptItem {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	id := ""
	if ev.Kind == protocol.EventStream {
		id = t.liveID
		// An empty text delta is the reasoning-close signal (Anthropic emitEnd):
		// it finalizes the open reasoning cell without starting an answer cell.
		// Project that reasoning cell so subscribers see it flip to "completed"
		// immediately rather than on the next full snapshot.
		if id == "" && ev.Text == "" {
			id = t.reasoningID
		}
	}
	if ev.Kind == protocol.EventReasoning {
		id = t.reasoningID
	}
	if ev.Kind == protocol.EventUserMessage {
		id = ev.MessageID
	}
	if ev.Tool != nil {
		id = toolEventTranscriptID(ev)
	}
	if id == "" {
		return nil
	}
	for _, item := range t.items {
		if item.ID == id {
			copy := item
			copy.Args = append(json.RawMessage(nil), item.Args...)
			return &copy
		}
	}
	return nil
}

func transcriptFromRecords(records []session.Record) *transcriptState {
	t := &transcriptState{}
	for _, record := range records {
		t.facts([]session.Event{record.Event})
	}
	return t
}
