package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

func TestLifecycleDeliveredImageCacheReleased(t *testing.T) {
	a, _ := formalCompletionAgent(t, &formalCompletionProvider{})
	cmd := protocol.NewSubmitInput("audit-image-cmd", protocol.SessionID(a.SessionID()), "audit-image", "inspect", protocol.InputSteer)
	cmd.Input.Images = []protocol.InputImage{inputPNG(t, 42)}
	if r, e := a.Submit(context.Background(), cmd); e != nil || r.Rejected() {
		t.Fatalf("submit: %+v %v", r, e)
	}
	waitJournalRuntimeIdle(t, a)
	if e := a.clearHistoryCommand("audit-clear"); e != nil {
		t.Fatal(e)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.inputAttachments["audit-image"]; ok {
		t.Fatalf("delivered and cleared image retained in pending cache: %d attachments, %d bytes", len(v.Attachments), len(v.Attachments[0].Data))
	}
}

func TestLifecycleCommandDrainsPendingInput(t *testing.T) {
	a, _ := formalCompletionAgent(t, &formalCompletionProvider{})
	gate := make(chan struct{})
	cmd := protocol.Command{ID: "audit-op", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandQuery, Query: &protocol.QueryCommand{Kind: protocol.QueryStatus}}
	r := a.scheduleCommandOperation(cmd, "audit", func(context.Context) (string, error) { <-gate; return "done", nil })
	if r.Rejected() {
		t.Fatal(r.Error)
	}
	next := protocol.NewSubmitInput("audit-next-cmd", protocol.SessionID(a.SessionID()), "audit-next", "continue after operation", protocol.InputSteer)
	r, e := a.Submit(context.Background(), next)
	close(gate)
	if e != nil || r.Rejected() {
		t.Fatalf("submit: %+v %v", r, e)
	}
	a.operationWG.Wait()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		v, _ := a.Snapshot(context.Background())
		if len(v.PendingInputs) == 0 && v.LastTurn != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, _ := a.Snapshot(context.Background())
	t.Fatalf("operation finished but input not consumed: busy=%v pending=%d lastTurn=%+v", v.Busy, len(v.PendingInputs), v.LastTurn)
}

func TestLifecycleCompactCurrentLongTurn(t *testing.T) {
	a := newRuntimeAgent(t)
	a.mu.Lock()
	a.cfg.KeepAfterCompact = 4
	a.history = []messages.Message{{Role: messages.RoleUser, Content: "review all code"}}
	for i := 0; i < 6; i++ {
		call := messages.ToolCall{ID: string(rune('a' + i)), Name: "Read", Arguments: map[string]any{"file_path": "large.go"}}
		a.history = append(a.history, messages.AssistantWithTools("inspect next file", []messages.ToolCall{call}), messages.NewToolResult(call, strings.Repeat("large step output ", 10000), false))
	}
	before := len(a.history)
	a.mu.Unlock()
	_, summarized, tail, _, _, ok := a.compactSpans()
	if !ok {
		t.Fatal("no compaction available")
	}
	if len(summarized) <= 1 && len(tail) == before-1 {
		t.Fatalf("only user prompt summarized; all %d large step outputs remain", len(tail))
	}
}

func TestLifecycleCompactFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "named-pipe")
	if e := syscall.Mkfifo(path, 0600); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := readBoundedRegularFile(path, 100); done <- e }()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("FIFO accepted")
		}
		return
	case <-time.After(150 * time.Millisecond):
	}
	// Release the blocked reader, so this reproduction leaves no leaked goroutine.
	writer, e := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		t.Fatal(e)
	}
	_ = writer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup failed")
	}
	t.Fatal("regular-file helper blocked opening FIFO before checking its type")
}

func TestLifecycleCheckpointFailureIsFailedTurn(t *testing.T) {
	p := &formalCompletionProvider{}
	a, _ := formalCompletionAgent(t, p)
	a.mu.Lock()
	a.cfg.Workspace = filepath.Join(t.TempDir(), "workspace-no-longer-present")
	a.mu.Unlock()
	cmd := protocol.NewSubmitInput("audit-checkpoint-cmd", protocol.SessionID(a.SessionID()), "audit-checkpoint-input", "continue", protocol.InputSteer)
	if r, e := a.Submit(context.Background(), cmd); e != nil || r.Rejected() {
		t.Fatalf("submit %+v %v", r, e)
	}
	v := waitJournalRuntimeIdle(t, a)
	if v.LastTurn.Status != protocol.TurnFailed {
		t.Fatalf("checkpoint failure reported %+v; provider calls=%d", v.LastTurn, p.requestsCount())
	}
}

func TestPartialProviderOutputDoesNotRestartOnFallback(t *testing.T) {
	provider := &callbackRetryProvider{}
	a := newJournalRuntimeAgent(t, provider)
	a.mu.Lock()
	a.fallbackBinding = modelBinding{model: "fallback-model", provider: provider.Name(), client: provider}
	a.mu.Unlock()
	submitJournalRuntimeInput(t, a)
	view := waitJournalRuntimeIdle(t, a)
	if provider.calls.Load() != 1 || view.LastTurn.Status != protocol.TurnFailed {
		t.Fatalf("partial response restarted: calls=%d outcome=%+v", provider.calls.Load(), view.LastTurn)
	}
}
