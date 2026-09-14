package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

type headlessTestSubscription struct {
	mu     sync.Mutex
	ch     chan protocol.Update
	closed bool
}

func (s *headlessTestSubscription) Updates() <-chan protocol.Update { return s.ch }
func (s *headlessTestSubscription) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	return nil
}

func (s *headlessTestSubscription) send(update protocol.Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.ch <- update
}

type headlessTestClient struct {
	mu             sync.Mutex
	snapshot       protocol.SessionView
	subs           []*headlessTestSubscription
	commands       []protocol.Command
	inputDeadlines []time.Time
	reject         bool
	transportErr   error
	blockWatch     bool
	blockInput     bool
	inputDelay     time.Duration
	resyncInput    bool
	watchLifetime  bool
}

func (c *headlessTestClient) Snapshot(context.Context) (protocol.SessionView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot, nil
}

func (c *headlessTestClient) Watch(ctx context.Context, _ protocol.Cursor) (protocol.Subscription, error) {
	if c.blockWatch {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c.mu.Lock()
	s := &headlessTestSubscription{ch: make(chan protocol.Update, 32)}
	snapshot := c.snapshot
	c.subs = append(c.subs, s)
	c.mu.Unlock()
	s.send(protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &snapshot, Revision: snapshot.Revision,
		Cursor: protocol.Cursor{LogSeq: snapshot.Revision.LogSeq, ViewGeneration: snapshot.Revision.ViewGeneration}})
	if c.watchLifetime {
		go func() {
			<-ctx.Done()
			_ = s.Close()
		}()
	}
	return s, nil
}

func (c *headlessTestClient) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	c.mu.Lock()
	c.commands = append(c.commands, cmd)
	reject, transportErr, blockInput := c.reject, c.transportErr, c.blockInput
	inputDelay := c.inputDelay
	if cmd.Type == protocol.CommandSubmitInput {
		if deadline, ok := ctx.Deadline(); ok {
			c.inputDeadlines = append(c.inputDeadlines, deadline)
		}
	}
	c.mu.Unlock()
	if blockInput && cmd.Type == protocol.CommandSubmitInput {
		<-ctx.Done()
		return protocol.Receipt{}, ctx.Err()
	}
	if inputDelay > 0 && cmd.Type == protocol.CommandSubmitInput {
		timer := time.NewTimer(inputDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return protocol.Receipt{}, ctx.Err()
		case <-timer.C:
		}
	}
	if transportErr != nil {
		return protocol.Receipt{}, transportErr
	}
	if reject {
		return protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: protocol.ReceiptRejected,
			Error: &protocol.CommandError{Code: protocol.ErrorBusy, Message: "busy"}}, nil
	}
	if cmd.Type == protocol.CommandSubmitInput {
		c.mu.Lock()
		resyncInput := c.resyncInput
		if resyncInput {
			c.snapshot.History = []protocol.MessageView{{ID: "failed-assistant", Role: "assistant", Content: "partial before failure"}}
			c.snapshot.Busy = false
			c.snapshot.LastTurn = &protocol.TurnOutcome{TurnID: "turn-failed", Status: protocol.TurnFailed, Error: "turn failed after resync"}
			c.resyncInput = false
		}
		c.mu.Unlock()
		if resyncInput {
			c.publish(protocol.Update{Type: protocol.UpdateResyncRequired, Cursor: protocol.Cursor{LogSeq: 2}})
			return protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: protocol.ReceiptApplied}, nil
		}
		c.publish(protocol.Update{Type: protocol.UpdateStream, Event: &protocol.EventView{
			Kind: protocol.EventStream, SessionID: cmd.SessionID, Text: "headless ok"}})
		c.publish(protocol.Update{Type: protocol.UpdateStream, Event: &protocol.EventView{
			Kind: protocol.EventTurnDone, SessionID: cmd.SessionID}})
	}
	return protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: protocol.ReceiptApplied}, nil
}

func (c *headlessTestClient) publish(update protocol.Update) {
	c.mu.Lock()
	subs := append([]*headlessTestSubscription(nil), c.subs...)
	c.mu.Unlock()
	for _, s := range subs {
		s.send(update)
	}
}

func (c *headlessTestClient) commandCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.commands)
}

func (c *headlessTestClient) inputDeadlineSnapshot() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.inputDeadlines...)
}

func captureHeadlessStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	os.Stderr = w
	code := fn()
	_ = w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	data, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data), code
}

func TestLoadConfigUsesDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ccdp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model":"from-default-config"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "from-default-config" {
		t.Fatalf("model = %q, want default config value", cfg.Model)
	}
}

func TestParseStreamJSONInput(t *testing.T) {
	in := `{"type":"user","message":{"role":"user","content":"first"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"second"}]}}
{"type":"assistant"}
`
	prompts, err := parseStreamJSONInput(bufio.NewReader(strings.NewReader(in)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(prompts) != 2 || prompts[0] != "first" || prompts[1] != "second" {
		t.Errorf("unexpected prompts: %+v", prompts)
	}
}

func TestParseStreamJSONInputEmpty(t *testing.T) {
	if _, err := parseStreamJSONInput(bufio.NewReader(strings.NewReader("{\"type\":\"assistant\"}\n"))); err == nil {
		t.Error("expected error for no user messages")
	}
}

func TestParseStreamJSONInputInvalidJSONReportsLine(t *testing.T) {
	input := "{\"type\":\"user\",\"message\":{\"content\":\"ok\"}}\n" +
		"{\"type\":\"user\",\"message\":{\"content\":}\n"
	_, err := parseStreamJSONInput(bufio.NewReader(strings.NewReader(input)))
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("invalid JSON error = %v, want line and parse context", err)
	}
}

func TestParseStreamJSONInputBoundsOversizedLine(t *testing.T) {
	old := maxStreamJSONLineBytes
	maxStreamJSONLineBytes = 32
	defer func() { maxStreamJSONLineBytes = old }()
	line := `{"type":"user","message":{"content":"` + strings.Repeat("x", 64) + `"}}` + "\n"
	if _, err := parseStreamJSONInput(bufio.NewReader(strings.NewReader(line))); err == nil {
		t.Fatal("oversized stream-json line was accepted")
	}
}

func TestContentText(t *testing.T) {
	if s := contentText([]byte(`"hello"`)); s != "hello" {
		t.Errorf("string content: %q", s)
	}
	if s := contentText([]byte(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)); s != "ab" {
		t.Errorf("array content: %q", s)
	}
}

func headlessTestSnapshot() protocol.SessionView {
	return protocol.SessionView{SessionID: "headless-test", Revision: protocol.Revision{LogSeq: 1},
		Settings: protocol.SettingsSnapshot{Model: protocol.ModelBinding{Model: "test-model"}}}
}

func TestRunHeadlessClientSuccessUsesWatchAndSubmit(t *testing.T) {
	client := &headlessTestClient{snapshot: headlessTestSnapshot(), watchLifetime: true}
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClient(client, []string{"hello"}, outText, 2)
	})
	if code != 0 || strings.TrimSpace(out) != "headless ok" {
		t.Fatalf("success output=%q code=%d", out, code)
	}
	if client.commandCount() != 1 {
		t.Fatalf("submit count=%d, want one input", client.commandCount())
	}
}

func TestRunHeadlessClientFailureIsReported(t *testing.T) {
	client := &headlessTestClient{snapshot: headlessTestSnapshot(), reject: true}
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClient(client, []string{"hello"}, outJSON, 2)
	})
	if code != 1 || !strings.Contains(out, `"error"`) || !strings.Contains(out, "busy") {
		t.Fatalf("failure output=%q code=%d", out, code)
	}
}

func TestRunHeadlessClientTimeoutCancelsSubmitAndStops(t *testing.T) {
	client := &headlessTestClient{snapshot: headlessTestSnapshot(), blockInput: true}
	started := time.Now()
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClient(client, []string{"hello"}, outText, 1)
	})
	if code != 2 || !strings.Contains(out, "timeout after 1s") {
		t.Fatalf("timeout output=%q code=%d", out, code)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("timeout path took too long: %s", time.Since(started))
	}
	if client.commandCount() != 2 {
		t.Fatalf("timeout should submit input and stop, got %d commands", client.commandCount())
	}
}

func TestRunHeadlessClientUsesOneOverallDeadlineForMultiplePrompts(t *testing.T) {
	// Each prompt takes less than the one-second budget, but two prompts cannot
	// both fit in it. A per-prompt timeout would let the second prompt finish;
	// the shared root deadline must terminate it instead.
	client := &headlessTestClient{snapshot: headlessTestSnapshot(), inputDelay: 650 * time.Millisecond}
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClient(client, []string{"first", "second"}, outText, 1)
	})
	if code != 2 || !strings.Contains(out, "timeout after 1s") {
		t.Fatalf("overall deadline output=%q code=%d", out, code)
	}
	deadlines := client.inputDeadlineSnapshot()
	if len(deadlines) < 2 {
		t.Fatalf("recorded input deadlines = %d, want both prompts", len(deadlines))
	}
	if deadlines[1].After(deadlines[0].Add(10 * time.Millisecond)) {
		t.Fatalf("second prompt received a fresh deadline: first=%s second=%s", deadlines[0], deadlines[1])
	}
}

func TestRunHeadlessClientSlowWatchHonorsStartupTimeout(t *testing.T) {
	client := &headlessTestClient{snapshot: headlessTestSnapshot(), blockWatch: true}
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClient(client, []string{"hello"}, outJSON, 1)
	})
	if code != 2 || !strings.Contains(out, "timeout after 1s") {
		t.Fatalf("slow watch output=%q code=%d", out, code)
	}
}

func TestRunHeadlessClientResyncUsesFailedLastTurn(t *testing.T) {
	client := &headlessTestClient{snapshot: headlessTestSnapshot(), resyncInput: true}
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClient(client, []string{"hello"}, outText, 2)
	})
	if code != 1 || !strings.Contains(out, "turn failed after resync") {
		t.Fatalf("failed resync output=%q code=%d", out, code)
	}
}

func TestRunHeadlessClientStreamJSONDoesNotPretendResyncSuccess(t *testing.T) {
	client := &headlessTestClient{snapshot: headlessTestSnapshot(), resyncInput: true}
	out, code := captureHeadlessStdout(t, func() int {
		return runHeadlessClient(client, []string{"hello"}, outStreamJSON, 2)
	})
	if code != 1 || !strings.Contains(out, "resynchronized before turn completion") {
		t.Fatalf("stream resync output=%q code=%d", out, code)
	}
}
