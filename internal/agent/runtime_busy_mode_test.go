package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestModeChangeWhileBusyThroughRunLoop drives a genuinely running turn through
// the real Submit()/Run() command channel (not a direct applyCommand call) and
// submits a permission-mode change mid-turn. It guards the reported bug where
// /mode was rejected during a run: the change must be ACCEPTED (not rejected, not
// deadlocked) and, because permission is a discrete-decision setting, applied to
// live state immediately rather than deferred to the next step boundary. The
// Submit call runs under a hard timeout so a deadlock on the command loop
// surfaces as a failure rather than hanging CI.
func TestModeChangeWhileBusyThroughRunLoop(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-started:
		default:
			close(started)
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.BaseURL = server.URL
	cfg.APIKey = "k"
	cfg.PermissionMode = string(permissions.ModeDefault)
	ag, err := New(&cfg, make(chan Event, 256))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	go ag.Run()

	sid := protocol.SessionID(ag.SessionID())
	if rcpt, err := ag.Submit(context.Background(),
		protocol.NewSubmitInput("repro-input", sid, "repro-input", "write something", protocol.InputSteer)); err != nil || rcpt.Rejected() {
		t.Fatalf("input submit: %+v err=%v", rcpt, err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider never started the turn")
	}

	done := make(chan protocol.Receipt, 1)
	go func() {
		rcpt, err := ag.Submit(context.Background(), protocol.Command{
			ID: "repro-mode", SessionID: sid, Type: protocol.CommandSetPermissionPolicy,
			PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeBypass)}},
		})
		if err != nil {
			t.Errorf("mode submit error: %v", err)
		}
		done <- rcpt
	}()

	select {
	case rcpt := <-done:
		if rcpt.Rejected() {
			t.Fatalf("mode change was rejected while busy: %+v", rcpt)
		}
		if rcpt.Status != protocol.ReceiptApplied {
			t.Fatalf("mode change while busy = %s, want applied (discrete settings apply immediately)", rcpt.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mode submit never returned a receipt while a turn was running")
	}

	// Permission is a discrete-decision setting: it lands on live state at once,
	// even though a provider request is streaming, because the running step froze
	// its own config and the change governs the next gate evaluation.
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.cfg.PermissionMode != string(permissions.ModeBypass) {
		t.Fatalf("permission mode did not apply immediately: %s", ag.cfg.PermissionMode)
	}
}
