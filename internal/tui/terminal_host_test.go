package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTerminalBarrierWaitsForCompleteUnderlyingWrite(t *testing.T) {
	var output bytes.Buffer
	host := NewTerminalHost(&output)
	_, marker, done := host.barrier()
	input := "history\r\n" + marker + "draft"
	for i := range input {
		if _, err := host.Write([]byte(input[i : i+1])); err != nil {
			t.Fatal(err)
		}
		if i < len("history\r\n")+len(marker)-1 {
			select {
			case <-done:
				t.Fatal("acknowledged before barrier reached writer")
			default:
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if output.String() != "history\r\ndraft" {
		t.Fatalf("leaked marker or lost bytes: %q", output.String())
	}
}

type shortTerminalWriter struct{}

func (shortTerminalWriter) Write(p []byte) (int, error) { return 0, nil }
func TestTerminalWriteFailureIsNotAcknowledgedAsSuccess(t *testing.T) {
	h := NewTerminalHost(shortTerminalWriter{})
	_, marker, done := h.barrier()
	if _, err := h.Write([]byte("history" + marker)); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

type hostProbe struct {
	host *TerminalHost
	ack  bool
	err  error
}

func (m hostProbe) Init() tea.Cmd {
	model := Model{terminal: m.host}
	return model.printHistory(inlinePrintMsg{generation: 1, batch: 1, text: "HISTORY_ONCE"})
}
func (m hostProbe) View() string { return "› draft" }
func (m hostProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ack, ok := msg.(inlinePrintedMsg); ok {
		m.ack = true
		m.err = ack.err
		return m, tea.Quit
	}
	return m, nil
}
func TestBubbleTeaBackendAcknowledgesActualHistoryWrite(t *testing.T) {
	var output bytes.Buffer
	h := NewTerminalHost(&output)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := tea.NewProgram(hostProbe{host: h}, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(h), tea.WithoutSignalHandler())
	model, err := p.Run()
	if err != nil {
		t.Fatal(err)
	}
	result := model.(hostProbe)
	if !result.ack || result.err != nil {
		t.Fatalf("missing write acknowledgment: %#v", result)
	}
	if strings.Count(output.String(), "HISTORY_ONCE") != 1 || strings.Contains(output.String(), "ccdp-") {
		t.Fatalf("bad terminal output: %q", output.String())
	}
}

func TestHistoryLedgerCommitsOnlyMatchingWriteAcknowledgment(t *testing.T) {
	m := inlineTestModel()
	m.routing = &sessionRouting{generation: 4}
	m.terminal = NewTerminalHost(io.Discard)
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.inline.showBaseline = false
	m.items = []historyCell{{kind: "user", messageID: "u1", text: "hello"}}
	cmd := m.flushInline()
	if cmd == nil || m.delivery.pending == nil {
		t.Fatal("missing prepared batch")
	}
	if m.inline.seen(m.items[0], 0) {
		t.Fatal("marked committed before write")
	}
	msg := cmd().(inlinePrintMsg)
	if err := m.acknowledgeHistory(inlinePrintedMsg{generation: 3, batch: msg.batch}); err != nil {
		t.Fatal(err)
	}
	if m.inline.seen(m.items[0], 0) {
		t.Fatal("stale ack committed history")
	}
	if m.flushInline() != nil {
		t.Fatal("duplicate pending batch")
	}
	if err := m.acknowledgeHistory(inlinePrintedMsg{generation: 4, batch: msg.batch}); err != nil {
		t.Fatal(err)
	}
	if !m.inline.seen(m.items[0], 0) || m.delivery.pending != nil {
		t.Fatal("matching ack did not commit")
	}
}
