package tui

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Force OS-style short reads through Bubble Tea's real input parser, rather
// than fabricating the KeyMsgs whose shape depends on the read boundary.
type terminalChunkReader struct{ chunks []string }

func (r *terminalChunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestTerminalSplitMouseDoesNotEnterComposer(t *testing.T) {
	const report = "\x1b[<65;62;14M"
	for split := 1; split < len(report); split++ {
		t.Run(report[:split], func(t *testing.T) {
			m := NewWithClient(nil, t.TempDir(), false)
			m.textarea.SetValue("draft")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			p := NewProgram(m, tea.WithContext(ctx), tea.WithoutRenderer(), tea.WithoutSignalHandler(),
				tea.WithInput(&terminalChunkReader{chunks: []string{report[:split], report[split:], "!", "\x03", "\x03"}}))
			final, err := p.Run()
			if err != nil {
				t.Fatal(err)
			}
			got := modelValue(t, final)
			if got.textarea.Value() != "draft!" {
				t.Fatalf("mouse report leaked into draft: %q", got.textarea.Value())
			}
			if got.mouseX != 61 || got.mouseY != 13 {
				t.Fatalf("mouse report was not dispatched: (%d,%d)", got.mouseX, got.mouseY)
			}
		})
	}
}

type terminalInputProbe struct{ messages []tea.Msg }

func (m terminalInputProbe) Init() tea.Cmd { return nil }
func (m terminalInputProbe) View() string  { return "" }
func (m terminalInputProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if batch, ok := msg.(terminalInputBatchMsg); ok {
		var next tea.Model = m
		var cmds []tea.Cmd
		for _, item := range batch {
			var cmd tea.Cmd
			next, cmd = next.Update(item)
			cmds = append(cmds, cmd)
		}
		return next, tea.Batch(cmds...)
	}
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyCtrlCloseBracket {
		return m, tea.Quit
	}
	switch msg.(type) {
	case tea.KeyMsg, tea.MouseMsg:
		m.messages = append(m.messages, msg)
	}
	return m, nil
}

func readTerminalMessages(t *testing.T, repair bool, chunks ...string) []tea.Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithoutRenderer(), tea.WithoutSignalHandler(),
		tea.WithOutput(io.Discard), tea.WithInput(&terminalChunkReader{chunks: append(chunks, "\x1d")})}
	var program *tea.Program
	if repair {
		input := &terminalInput{send: func(msg tea.Msg) { program.Send(msg) }}
		options = append(options, tea.WithFilter(input.filter))
	}
	program = tea.NewProgram(terminalInputProbe{}, options...)
	final, err := program.Run()
	if err != nil {
		t.Fatal(err)
	}
	return final.(terminalInputProbe).messages
}

func TestTerminalSplitMouseMatchesNativeEvents(t *testing.T) {
	// Compare reconstructed events against the actual library's decoder,
	// including modifiers, drag motion, release and extended buttons.
	for _, button := range []int{0, 1, 2, 3, 4, 8, 16, 28, 32, 35, 64, 65, 66, 67, 92, 128, 129, 130, 131, 160, 163} {
		for _, end := range []string{"M", "m"} {
			report := fmt.Sprintf("\x1b[<%d;62;14%s", button, end)
			want := readTerminalMessages(t, false, report)
			for split := 1; split < len(report); split++ {
				got := readTerminalMessages(t, true, report[:split], report[split:])
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%q split at %d:\n got %#v\nwant %#v", report, split, got, want)
				}
			}
		}
	}
}

func TestTerminalMouseBurstAcrossReadBuffer(t *testing.T) {
	const report = "\x1b[<65;62;14M"
	// Position the report at every possible offset in Bubble Tea's 256-byte
	// buffer. A long wheel burst exercises its leftover-data path as well.
	for padding := 0; padding < 256; padding++ {
		got := readTerminalMessages(t, true, strings.Repeat("x", padding)+strings.Repeat(report, 100)+"end")
		var text strings.Builder
		count := 0
		for _, msg := range got {
			switch msg := msg.(type) {
			case tea.MouseMsg:
				count++
				if msg.Button != tea.MouseButtonWheelDown || msg.X != 61 || msg.Y != 13 {
					t.Fatalf("incorrect mouse event: %#v", msg)
				}
			case tea.KeyMsg:
				text.WriteString(string(msg.Runes))
			}
		}
		if count != 100 || text.String() != strings.Repeat("x", padding)+"end" {
			t.Fatalf("padding %d: %d mouse events, text %q", padding, count, text.String())
		}
	}
}

func TestTerminalMouseRepairPreservesKeyboardAndPaste(t *testing.T) {
	for _, chunks := range [][]string{
		{"[<65;62;14M"}, // Literal text is never stripped.
		{"\x1b[200~[<65;62;14M\x1b[<65;62;14M\x1b[201~"},
		{"\x1b[200~", "\x1b[<65;", "62;14M", "\x1b[201~"},
		{"中文", "\x1b[A", "\x1b[B", "\x1b[D", "\x1b[C", "\x1bx", "\x1b\r"},
		{"\x1b", "\x1b"}, // Double Escape must still clear the draft.
		{"\x1b\x1b"},
		{"\x1b[", "not-a-report"},
		{"\x1b[<", "65;2;x"},
		{"\x1b[<", strings.Repeat("9", 1000)},
		{"\x1b[", "\x1b[<64;1;1M"},
	} {
		want := readTerminalMessages(t, false, chunks...)
		got := readTerminalMessages(t, true, chunks...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%q:\n got %#v\nwant %#v", chunks, got, want)
		}
	}
}

func TestTerminalMouseBytewiseAndTextSuffix(t *testing.T) {
	const report = "\x1b[<64;62;14M"
	want := readTerminalMessages(t, false, report, "中文")
	for _, chunks := range [][]string{
		strings.Split(report+"中文", ""),
		{"\x1b[", "<64;62;14M中文"},
	} {
		got := readTerminalMessages(t, true, chunks...)
		// KeyRunes grouping may differ; concatenate only the typed suffix.
		if len(got) < 2 || !reflect.DeepEqual(got[0], want[0]) {
			t.Fatalf("missing reconstructed mouse: %#v", got)
		}
		var suffix strings.Builder
		for _, msg := range got[1:] {
			key, ok := msg.(tea.KeyMsg)
			if !ok || key.Alt || key.Paste {
				t.Fatalf("bad suffix: %#v", msg)
			}
			suffix.WriteString(string(key.Runes))
		}
		if suffix.String() != "中文" {
			t.Fatalf("lost suffix: %q", suffix.String())
		}
	}
}

func TestTerminalEscapeTimeoutAndStaleTimers(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyRunes, Alt: true, Runes: []rune{'['}}} {
		timeouts := make(chan tea.Msg, 4)
		input := &terminalInput{send: func(msg tea.Msg) { timeouts <- msg }}
		defer input.flush()
		if got := input.filter(nil, key); got != nil {
			t.Fatalf("prefix was not held: %#v", got)
		}
		stale := terminalInputTimeoutMsg(input.serial)
		// Unrelated rendering must not flush the sequence into the composer.
		resize := tea.WindowSizeMsg{Width: 80, Height: 24}
		if got := input.filter(nil, resize); got != resize {
			t.Fatalf("resize changed: %#v", got)
		}
		select {
		case timeout := <-timeouts:
			if got := input.filter(nil, timeout); !reflect.DeepEqual(got, key) {
				t.Fatalf("standalone key not restored: %#v", got)
			}
		case <-time.After(time.Second):
			t.Fatal("standalone key never flushed")
		}
		input.filter(nil, key)
		if got := input.filter(nil, stale); got != nil || input.prefix == "" {
			t.Fatalf("stale timeout flushed new input: %#v", got)
		}
	}
}
