package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

type captureClipboard struct {
	text string
	err  error
}

type contextBlockingClipboard struct{}

func (contextBlockingClipboard) WriteAllContext(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func inlineTestModel() *Model {
	m := sugModel()
	m.inlineMode = true
	return m
}

func (c *captureClipboard) WriteAllContext(_ context.Context, text string) error {
	c.text = text
	return c.err
}

func TestScrollStateClampsAndKeepsSelectionVisible(t *testing.T) {
	var state ScrollState
	state.Set(3, 20)
	state.Move(100)
	if state.Offset != 17 {
		t.Fatalf("offset=%d, want max 17", state.Offset)
	}
	state.PageUp()
	if state.Offset != 14 {
		t.Fatalf("page up offset=%d, want 14", state.Offset)
	}
	state.EnsureVisible(1)
	if state.Offset != 1 {
		t.Fatalf("ensure visible offset=%d, want 1", state.Offset)
	}
	state.End()
	state.Set(5, 2)
	if state.Offset != 0 || state.MaxOffset() != 0 {
		t.Fatalf("resize did not clamp: %#v", state)
	}
}

func TestPickerSanitizesANSIInTitleAndOptions(t *testing.T) {
	m := sugModel()
	m.width, m.height = 80, 20
	m.startSelectorAt("Pick \x1b]52;c;secret\a", []SelectorOption{{ID: "one", Label: "safe \x1b[2Jentry"}}, 0, false, selectorAction{Kind: selectorModel})
	out := m.renderInlineSurface()
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\a') {
		t.Fatalf("picker leaked terminal controls: %q", out)
	}
	if !strings.Contains(out, "safe") || !strings.Contains(out, "entry") {
		t.Fatalf("picker lost sanitized option text: %q", out)
	}
	m.picker.inline = true
	out = m.renderInlineSurface()
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\a') {
		t.Fatalf("inline picker leaked terminal controls: %q", out)
	}
}

func TestManagedHistoryRemainsVisibleAfterFlush(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 80, 24
	m.items = []historyCell{{kind: "assistant", messageID: "old", text: "old answer"}}
	m.inline.forgetAll()
	m.inline.prime(m.items)
	m.inline.showInitialFrame()
	m.layout()
	if got := m.View(); !strings.Contains(got, "old answer") {
		t.Fatalf("initial managed frame hid existing history: %q", got)
	}
	m.items = append(m.items, historyCell{kind: "user", messageID: "new", text: "new prompt"})
	m.turnDone = false
	if cmd := planTestHistory(m); cmd == nil {
		t.Fatal("new complete user unit should be queued to native scrollback")
	}
	m.layout()
	if got := m.View(); !strings.Contains(got, "old answer") {
		t.Fatalf("visible history disappeared after flush: %q", got)
	}
}

func TestInlineInitialHistoryIsFullyCommittedBeforeTailDrops(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 80, 24
	m.items = make([]historyCell, 48)
	for i := range m.items {
		m.items[i] = historyCell{kind: "assistant", messageID: "history-" + strconv.Itoa(i), text: "history line " + strconv.Itoa(i)}
	}
	m.inline.forgetAll()
	m.inline.prime(m.items)
	m.inline.showInitialFrame()
	m.layout()
	if got := m.View(); !strings.Contains(got, "history line 47") || strings.Contains(got, "history line 0") {
		t.Fatalf("initial live frame should show only the bounded tail: %q", got)
	}
	if cmd := planTestHistory(m); cmd == nil {
		t.Fatal("initial history was not sent to native scrollback")
	}
	if len(m.inline.printed) != len(m.items) {
		t.Fatalf("committed %d of %d initial history items", len(m.inline.printed), len(m.items))
	}
	if cmd := planTestHistory(m); cmd != nil {
		t.Fatal("initial history was emitted twice")
	}
}

func TestInlineStableIDsDoNotCollapseEqualText(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 80, 24
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.inline.showBaseline = false
	m.turnDone = true
	m.items = []historyCell{
		{kind: "assistant", messageID: "a-1", text: "same answer"},
		{kind: "assistant", messageID: "a-2", text: "same answer"},
	}
	if cmd := planTestHistory(m); cmd == nil {
		t.Fatal("first equal-text batch should be emitted")
	}
	if cmd := planTestHistory(m); cmd != nil {
		t.Fatal("replaying equal-text batch with stable IDs emitted twice")
	}
	m.items = append(m.items, historyCell{kind: "assistant", messageID: "a-3", text: "same answer"})
	if cmd := planTestHistory(m); cmd == nil {
		t.Fatal("a distinct stable ID must be emitted even with equal content")
	}
}

func TestInlineLongStreamPrintsPrefixThenOnlyFinalDelta(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 80, 24
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.inline.showBaseline = false
	m.streaming = true
	m.turnDone = false
	m.items = []historyCell{{kind: "assistant", messageID: "stream-1", status: "streaming", text: strings.Repeat("x", inlinePrefixBytes)}}
	if cmd := planTestHistory(m); cmd == nil {
		t.Fatal("long active stream prefix was not emitted")
	}
	if got := m.inline.offsets["message:stream-1"]; got <= 0 || got >= len(m.items[0].text) {
		t.Fatalf("stream offset=%d, want a committed prefix with a live tail", got)
	}
	m.items[0].text += "tail"
	m.turnDone = true
	m.items[0].status = "completed"
	if cmd := planTestHistory(m); cmd == nil {
		t.Fatal("final stream delta was not emitted")
	}
	if _, ok := m.inline.offsets["message:stream-1"]; ok {
		t.Fatal("finalized stream retained an active offset")
	}
}

func TestInlineLongStreamKeepsSentenceTailAcrossChunks(t *testing.T) {
	m := inlineTestModel()
	m.width, m.height = 24, 24
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.inline.showBaseline = false
	m.streaming = true
	m.turnDone = false
	m.items = []historyCell{{kind: "assistant", messageID: "stream-1", status: "streaming", text: "hello " + strings.Repeat("longrun", 400) + "world"}}
	if cmd := planTestHistory(m); cmd == nil {
		t.Fatal("long stream prefix was not emitted")
	}
	offset := m.inline.offsets["message:stream-1"]
	if offset <= 0 || offset >= len(m.items[0].text) {
		t.Fatalf("stream prefix offset=%d, text bytes=%d; want a committed prefix and live tail", offset, len(m.items[0].text))
	}
	m.layout()
	if !strings.Contains(m.View(), "world") {
		t.Fatalf("live frame lost the sentence tail: %q", m.View())
	}
	m.turnDone = true
	m.items[0].status = "completed"
	if cmd := planTestHistory(m); cmd == nil {
		t.Fatal("final sentence suffix was not emitted")
	}
}

func TestInlineInterruptedStreamRemainsIdentified(t *testing.T) {
	m := inlineTestModel()
	m.inline.prime(nil)
	m.items = []historyCell{{kind: "assistant", messageID: "stream-interrupted", status: "interrupted", text: "partial"}}
	if planTestHistory(m) == nil {
		t.Fatal("interrupted output lost")
	}
	if planTestHistory(m) != nil {
		t.Fatal("interrupted output duplicated")
	}
}

func TestReaderCopyUsesRawHostClipboardText(t *testing.T) {
	m := inlineTestModel()
	copy := &captureClipboard{}
	m.clipboard = copy
	raw := "  answer\n\x1b[31mnot rendered\x1b[0m\n你好🙂"
	m.reader = &readerState{rawText: raw}
	cmd := m.copyReaderSource()
	if cmd == nil {
		t.Fatal("copy command was nil")
	}
	model, _ := m.Update(cmd())
	updated := modelValue(t, model)
	m = &updated
	if copy.text != raw {
		t.Fatalf("clipboard text=%q, want raw %q", copy.text, raw)
	}
	if noticeText(m) != "copied to clipboard" {
		t.Fatalf("copy status=%q", noticeText(m))
	}
}

func TestCopyReportsAdapterError(t *testing.T) {
	m := inlineTestModel()
	m.clipboard = &captureClipboard{err: errors.New("no pasteboard")}
	m.reader = &readerState{rawText: "answer"}
	cmd := m.copyReaderSource()
	if cmd == nil {
		t.Fatal("copy command was nil")
	}
	model, _ := m.Update(cmd())
	updated := modelValue(t, model)
	m = &updated
	if !strings.Contains(noticeText(m), "copy failed") {
		t.Fatalf("copy error status=%q", noticeText(m))
	}
}

func TestCopyTimesOutAndDoesNotCrossSessionToast(t *testing.T) {
	previous := clipboardWriteTimeout
	clipboardWriteTimeout = 10 * time.Millisecond
	defer func() { clipboardWriteTimeout = previous }()

	m := inlineTestModel()
	m.sessionID = "session-a"
	m.reportGeneration = 4
	m.clipboard = contextBlockingClipboard{}
	m.reader = &readerState{rawText: "answer"}
	cmd := m.copyReaderSource()
	if cmd == nil {
		t.Fatal("copy command was nil")
	}
	// The result is produced after the bounded context expires. A session
	// switch before delivery must discard the stale notice.
	m.sessionID = "session-b"
	model, _ := m.Update(cmd())
	updated := modelValue(t, model)
	m = &updated
	if noticeText(m) != "" {
		t.Fatalf("stale clipboard result crossed session boundary: %q", noticeText(m))
	}
}

func TestCopyCommandIsRemoved(t *testing.T) {
	m := inlineTestModel()
	m.confirmedItems = []historyCell{{kind: "assistant", text: "answer", messageID: "answer-1"}}
	m.clipboard = &captureClipboard{}
	if _, cmd := m.runCommand("/copy"); cmd != nil {
		t.Fatal("/copy should no longer return a clipboard command")
	}
	if _, ok := commandCatalog.Lookup("copy"); ok {
		t.Fatal("/copy should be gone from the command catalog")
	}
}

func TestBusyMutationUsesCatalogAdmission(t *testing.T) {
	m := sugModel()
	m.busy = true
	before := m.client.(*recordingClient).submitCount()
	_, cmd := m.runCommand("/clear")
	if cmd != nil || m.client.(*recordingClient).submitCount() != before {
		t.Fatal("busy /clear bypassed catalog admission")
	}
	if !strings.Contains(noticeText(m), "busy") {
		t.Fatalf("busy rejection status=%q", noticeText(m))
	}
}

func TestProtocolSnapshotKeepsRawMessageForCopy(t *testing.T) {
	m := NewWithClient(&recordingClient{snapshot: protocolSnapshot("copy-session", protocol.MessageView{
		ID: "assistant-1", Role: "assistant", Content: "你好🙂",
	})}, t.TempDir(), false)
	if len(m.confirmedItems) != 1 || m.confirmedItems[0].messageID != "assistant-1" {
		t.Fatalf("snapshot history missing stable message: %#v", m.confirmedItems)
	}
	if m.confirmedItems[0].text != "你好🙂" {
		t.Fatal("snapshot did not preserve wide/emoji content")
	}
}

func TestStableMessageIDsSurviveResyncWithoutReprint(t *testing.T) {
	client := &recordingClient{snapshot: protocolSnapshot("s", protocol.MessageView{ID: "a", Role: "assistant", Content: "done"})}
	m := NewWithClient(client, t.TempDir(), false)
	if cmd := planTestHistory(&m); cmd == nil {
		t.Fatal("initial stable history should be emitted to native scrollback")
	}
	if cmd := planTestHistory(&m); cmd != nil {
		t.Fatal("replaying initial baseline emitted history twice")
	}
	s := client.snapshot
	s.Revision.LogSeq++
	m.applySnapshot(s)
	if cmd := planTestHistory(&m); cmd != nil {
		t.Fatal("identical resync reprinted stable history")
	}
	if len(m.items) != 1 || m.items[0].messageID != "a" {
		t.Fatalf("resync changed stable history: %#v", m.items)
	}
	_ = protocol.CommandInterrupt
}
