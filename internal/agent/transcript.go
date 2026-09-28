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

// A rolling live window, not a model output limit. Complete messages remain
// in the journal. Tool arguments use a smaller structured-payload preview.
const (
	transcriptTextLimit = 256 << 10
	transcriptArgsLimit = 16 << 10
)

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
	buffers           map[string]*transcriptBuffer
	index             map[string]uint64
	before            uint64
	window            int
	mu                sync.Mutex
	items             []protocol.TranscriptItem
	more              bool
	reasoningID       string
	closedReasoningID string
	previewIDs        []string
	liveID            string
	serial            uint64
}

// Keep append work linear. Snapshots materialize only the visible tail; spare
// capacity amortizes front removal rather than copying 256 KiB per token.
type transcriptBuffer struct {
	data   []byte
	offset int64
}

func (t *transcriptState) appendLive(item protocol.TranscriptItem, delta string) {
	if t.buffers == nil {
		t.buffers = map[string]*transcriptBuffer{}
	}
	b := t.buffers[item.ID]
	if b == nil {
		b = &transcriptBuffer{data: []byte(item.Text), offset: item.TextOffset}
	}
	b.data = append(b.data, delta...)
	if len(b.data) > 2*transcriptTextLimit {
		drop := len(b.data) - transcriptTextLimit
		for drop < len(b.data) && !utf8.RuneStart(b.data[drop]) {
			drop++
		}
		b.offset += int64(drop)
		b.data = append([]byte(nil), b.data[drop:]...)
	}
	item.Text = ""
	t.put(item)
	t.buffers[item.ID] = b
}

func (t *transcriptState) materialize(item protocol.TranscriptItem) protocol.TranscriptItem {
	if b := t.buffers[item.ID]; b != nil {
		start := max(0, len(b.data)-transcriptTextLimit)
		for start < len(b.data) && !utf8.RuneStart(b.data[start]) {
			start++
		}
		item.Text = string(b.data[start:])
		item.TextOffset = b.offset + int64(start)
		item.Truncated = item.Truncated || item.TextOffset > 0
	}
	return item
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

func transcriptTail(s string, limit int) (string, int64) {
	start := max(0, len(s)-limit)
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:], int64(start)
}

func (t *transcriptState) put(item protocol.TranscriptItem) {
	delete(t.buffers, item.ID)
	delete(t.buffers, item.PreviousID)
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
	if (item.Kind == "assistant" || item.Kind == "thinking") && len(item.Text) > transcriptTextLimit {
		var dropped int64
		item.Text, dropped = transcriptTail(item.Text, transcriptTextLimit)
		item.TextOffset += dropped
		item.Truncated = true
	} else {
		var clipped bool
		item.Text, clipped = clipTranscript(item.Text)
		item.Truncated = item.Truncated || clipped
	}
	item.Args = append(json.RawMessage(nil), item.Args...)
	if len(item.Args) > transcriptArgsLimit {
		item.Args = nil
		item.Truncated = true
	}
	// Forwarded progress can change from a live ID to its durable ID without
	// passing through applyAssistantCommitted. Preserve the original position.
	if item.PreviousID != "" && item.PreviousID != item.ID {
		found := false
		kept := t.items[:0]
		for _, old := range t.items {
			if old.ID == item.PreviousID || old.ID == item.ID {
				if !found {
					kept = append(kept, item)
					found = true
				}
			} else {
				kept = append(kept, old)
			}
		}
		t.items = kept
		if found {
			return
		}
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
		for _, old := range t.items[:len(t.items)-window] {
			delete(t.buffers, old.ID)
		}
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
		items[i] = t.materialize(items[i])
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
		t.buffers = nil
		t.more = false
		t.liveID, t.reasoningID, t.closedReasoningID = "", "", ""
		t.previewIDs = nil
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
	kind := "user"
	if e.Strategy == string(protocol.InputMessage) {
		kind = "collaboration"
	}
	t.put(protocol.TranscriptItem{ID: id, Kind: kind, Text: e.Text, Status: "queued"})
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
	segmented := e.Message.Role == "assistant" && t.commitSegments(e, text.String())
	if !segmented && e.Message.Role == "assistant" && e.Message.ReasoningContent != "" {
		id := "reasoning:" + e.Message.MessageID
		previous := t.reasoningID
		if previous == "" {
			previous = t.closedReasoningID
		}
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
	if text.Len() > 0 && !segmented {
		t.put(protocol.TranscriptItem{ID: e.Message.MessageID, PreviousID: previousID, Kind: e.Message.Role, TurnID: protocol.TurnID(e.TurnID), Text: text.String(), Status: "completed"})
	}
	if e.Message.Role == "assistant" {
		t.previewIDs = nil
		t.closedReasoningID = ""
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
	for i := range t.items {
		for _, id := range t.previewIDs {
			if t.items[i].ID == id && t.items[i].Status == "streaming" {
				t.items[i].Status = "interrupted"
			}
		}
	}
	t.previewIDs = nil
	t.reasoningID, t.closedReasoningID = "", ""
	if e.Error != "" {
		t.put(protocol.TranscriptItem{ID: "error:" + e.TurnID, Kind: "error", Text: e.Error, TurnID: protocol.TurnID(e.TurnID), Status: e.Outcome})
	}
	if e.DurationMs > 0 {
		t.put(protocol.TranscriptItem{ID: "turn-summary:" + e.TurnID, Kind: "turn_summary", TurnID: protocol.TurnID(e.TurnID), Status: e.Outcome, DurationMs: e.DurationMs})
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
		case protocol.EventStream, protocol.EventToolStarted, protocol.EventTurnDone, protocol.EventUserMessage, protocol.EventError:
			t.closedReasoningID = t.reasoningID
			t.reasoningID = ""
		}
	}
	switch ev.Kind {
	case protocol.EventReasoning:
		if ev.Text == "" {
			return
		}
		for _, old := range t.items {
			if old.ID == t.reasoningID && old.TurnID != ev.TurnID {
				t.reasoningID = ""
				break
			}
		}
		if t.liveID != "" {
			t.closePreview(t.liveID)
			t.liveID = ""
		}
		if t.reasoningID == "" {
			t.serial++
			t.reasoningID = fmt.Sprintf("reasoning:%s:%d", ev.TurnID, t.serial)
			t.previewIDs = append(t.previewIDs, t.reasoningID)
		}
		item := protocol.TranscriptItem{ID: t.reasoningID, Kind: "thinking", TurnID: ev.TurnID, Status: "streaming"}
		for _, old := range t.items {
			if old.ID == item.ID {
				item = old
				break
			}
		}
		t.appendLive(item, ev.Text)
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
			t.previewIDs = append(t.previewIDs, t.liveID)
		}
		item := protocol.TranscriptItem{ID: t.liveID, Kind: "assistant", TurnID: ev.TurnID, Status: "streaming"}
		for _, old := range t.items {
			if old.ID == item.ID {
				item = old
				break
			}
		}
		t.appendLive(item, ev.Text)
	case protocol.EventToolProgress:
		if ev.Tool == nil {
			return
		}
		id := toolEventTranscriptID(ev)
		for _, old := range t.items {
			if old.ID == id && old.Status == "running" {
				t.appendLive(old, ev.Tool.Output)
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
			id = t.closedReasoningID
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
			copy := t.materialize(item)
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
