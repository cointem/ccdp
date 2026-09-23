package tui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

// TerminalHost is the sole output boundary for the Bubble Tea backend.
// Println enqueues output; it does not acknowledge a write. Private barriers are
// removed here and acknowledged only after the underlying writer accepts bytes.
type TerminalHost struct {
	mu      sync.Mutex
	output  io.Writer
	prefix  string
	tail    string
	next    uint64
	pending map[uint64]chan error
	failure error
	onError func(error)
}

func NewTerminalHost(output io.Writer) *TerminalHost {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic(err)
	}
	return &TerminalHost{output: output, prefix: "\x1b]ccdp-" + hex.EncodeToString(nonce[:]) + ";", pending: make(map[uint64]chan error)}
}

// Fd preserves terminal capability detection through the output adapter.
func (h *TerminalHost) Fd() uintptr {
	if f, ok := h.output.(interface{ Fd() uintptr }); ok {
		return f.Fd()
	}
	return ^uintptr(0)
}

// Bubble Tea checks the complete term.File interface, not Fd alone.
var _ term.File = (*TerminalHost)(nil)

func (h *TerminalHost) Read(p []byte) (int, error) {
	if reader, ok := h.output.(io.Reader); ok {
		return reader.Read(p)
	}
	return 0, io.EOF
}
func (h *TerminalHost) Close() error { return h.close() }

func (h *TerminalHost) barrier() (uint64, string, <-chan error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	done := make(chan error, 1)
	if h.failure != nil {
		done <- h.failure
	} else {
		h.pending[h.next] = done
	}
	return h.next, h.prefix + strconv.FormatUint(h.next, 10) + "\a", done
}

// WriteRaw emits bytes verbatim, bypassing the marker framing. It is used for
// self-contained sequences such as OSC 52 clipboard writes that must not be
// confused with an in-band barrier. Serialized under the same mutex as Write
// so it never interleaves with a frame.
func (h *TerminalHost) WriteRaw(p string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failure != nil {
		return
	}
	_, _ = io.WriteString(h.output, p)
}

func (h *TerminalHost) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failure != nil {
		return 0, h.failure
	}
	input := h.tail + string(p)
	h.tail = ""
	var out strings.Builder
	var completed []uint64
	for len(input) > 0 {
		start := strings.Index(input, h.prefix)
		if start < 0 {
			// A write can end in the middle of the marker prefix.
			keep := 0
			for n := min(len(input), len(h.prefix)-1); n > 0; n-- {
				if strings.HasSuffix(input, h.prefix[:n]) {
					keep = n
					break
				}
			}
			out.WriteString(input[:len(input)-keep])
			h.tail = input[len(input)-keep:]
			break
		}
		out.WriteString(input[:start])
		input = input[start:]
		end := strings.IndexByte(input, '\a')
		if end < 0 {
			h.tail = input
			break
		}
		marker := input[:end+1]
		id, err := strconv.ParseUint(input[len(h.prefix):end], 10, 64)
		if _, ok := h.pending[id]; err == nil && ok {
			completed = append(completed, id)
		} else {
			out.WriteString(marker)
		}
		input = input[end+1:]
	}
	data := out.String()
	n, err := io.WriteString(h.output, data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		h.failure = err
		if h.onError != nil {
			h.onError(err)
		}
		for id, done := range h.pending {
			done <- err
			delete(h.pending, id)
		}
		return 0, err
	}
	for _, id := range completed {
		if done, ok := h.pending[id]; ok {
			done <- nil
			delete(h.pending, id)
		}
	}
	return len(p), nil
}

func (h *TerminalHost) close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, done := range h.pending {
		done <- io.ErrClosedPipe
		delete(h.pending, id)
	}
	return h.failure
}

// NewProgram installs the terminal boundary for every production TUI.
func NewProgram(model Model, options ...tea.ProgramOption) *tea.Program {
	host := NewTerminalHost(os.Stdout)
	model.terminal = host
	options = append(options, tea.WithOutput(host))
	program := tea.NewProgram(model, options...)
	host.onError = func(err error) { go program.Send(terminalErrorMsg{err: err}) }
	return program
}

func (m *Model) printHistory(item inlinePrintMsg) tea.Cmd {
	if m.terminal == nil {
		return func() tea.Msg { return terminalErrorMsg{err: fmt.Errorf("terminal host unavailable")} }
	}
	_, marker, done := m.terminal.barrier()
	return tea.Sequence(tea.Println(item.text+marker), func() tea.Msg {
		err := <-done
		return inlinePrintedMsg{generation: item.generation, batch: item.batch, err: err}
	})
}

type historyCommit struct {
	id         uint64
	generation uint64
	next       historyLedger
}
type historyDelivery struct {
	sequence uint64
	pending  *historyCommit
}

func (t historyLedger) clone() historyLedger {
	t.inheritedPrinted = t.printed
	t.printed = make(map[string]struct{})
	offsets := make(map[string]int, len(t.offsets))
	for k, v := range t.offsets {
		offsets[k] = v
	}
	t.offsets = offsets
	return t
}

func (m *Model) acknowledgeHistory(msg inlinePrintedMsg) error {
	pending := m.delivery.pending
	if pending == nil || pending.id != msg.batch || pending.generation != msg.generation {
		return nil
	}
	if msg.err != nil {
		return fmt.Errorf("terminal history write: %w", msg.err)
	}
	if m.inline.epoch == pending.next.epoch {
		m.inline.accept(pending.next)
	}
	m.delivery.pending = nil
	// Flushed cells leave the managed frame, which renumbers content rows; a
	// selection anchored to the old numbering would highlight the wrong text.
	m.selActive = false
	m.selAnchor = nil
	m.selFocus = nil
	return nil
}

// accept merges only the newly written IDs. Previously committed history is
// shared read-only while planning, avoiding a full-history copy per delta.
func (t *historyLedger) accept(next historyLedger) {
	t.ensure()
	for key := range next.printed {
		t.printed[key] = struct{}{}
	}
	next.printed = t.printed
	next.inheritedPrinted = nil
	*t = next
}

type terminalErrorMsg struct{ err error }
