package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

type ignoringProgramTool struct {
	codeFixtureTool
	started, release, returned chan struct{}
}

func (t *ignoringProgramTool) Execute(*tools.Context, map[string]any) (tools.Result, error) {
	close(t.started)
	defer close(t.returned)
	<-t.release
	return tools.Result{Text: "late success"}, nil
}

func TestProgramCancellationSettlesUnknownOnce(t *testing.T) {
	a, _, _ := codeAgent(t, "", func(cfg *config.Config) { cfg.AlwaysAllow = []string{"Ignoring"} })
	tool := &ignoringProgramTool{codeFixtureTool: codeFixtureTool{name: "Ignoring"}, started: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	a.services.registry.Register(tool)
	defer func() {
		close(tool.release)
		select {
		case <-tool.returned:
		case <-time.After(time.Second):
		}
	}()
	step := testToolStep(t, a)
	jc := toolJournalContextForStep(step)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var result protocol.CodeResult
	var err error
	go func() {
		result, err = a.runCode(ctx, messages.ToolCall{ID: "cancelled-cell"}, jc, `await tools.Ignoring({})`, 10*time.Second)
		close(done)
	}()
	select {
	case <-tool.started:
	case <-done:
		t.Fatalf("tool not started: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup timed out")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tool blocked cell shutdown")
	}
	if err == nil || result.Unknown != 1 {
		t.Fatalf("lost uncertain outcome: %+v %v", result, err)
	}
	records, e := a.persistenceHandle().Read(session.Beginning)
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	for _, record := range records {
		if fact, ok := record.Event.(*session.ToolFinished); ok && fact.ParentCallID == "cancelled-cell" {
			count++
			if fact.Status != "unknown" || fact.SideEffect != "attempted" {
				t.Fatalf("claimed completed effect: %+v", fact)
			}
		}
	}
	if count != 1 || a.persistenceFailure() != nil {
		t.Fatalf("finished=%d persistence=%v", count, a.persistenceFailure())
	}
}

func TestRecoveryScansHonorCancelledRoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := config.Default()
	a := &Agent{}
	a.services.cfg = &cfg
	a.lifecycle.rootCtx = ctx
	if err := a.reconcileDescendantUsage(); !errors.Is(err, context.Canceled) {
		t.Fatalf("usage recovery ignored cancellation: %v", err)
	}
	if _, err := a.recoverGoalAccountingState(); !errors.Is(err, context.Canceled) {
		t.Fatalf("goal recovery ignored cancellation: %v", err)
	}
	if s := newSessionSupervisor(a, nil); !errors.Is(s.recoveryErr, context.Canceled) {
		t.Fatalf("supervisor recovery ignored cancellation: %v", s.recoveryErr)
	}
}
