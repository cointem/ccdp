package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestDelegatedResultsResumeSameParentTurnWithoutReadOrWait(t *testing.T) {
	release := map[string]chan struct{}{"child-a": make(chan struct{}), "child-b": make(chan struct{})}
	once := map[string]*sync.Once{"child-a": {}, "child-b": {}}
	finish := func(name string) { once[name].Do(func() { close(release[name]) }) }
	defer finish("child-a")
	defer finish("child-b")
	started := make(chan string, 2)
	parentSteps := make(chan int, 20)
	p := &childRuntimeTestProvider{name: "collaboration-events"}
	p.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		first, toolResults, finals := "", 0, 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults++
			}
			if m.Role == "user" {
				text, _ := m.Content.(string)
				if first == "" {
					first = text
				}
				if msg, ok := protocol.DecodeCollaboration(text); ok && msg.Kind == "final_result" {
					finals++
				}
			}
		}
		if gate, child := release[first]; child {
			if toolResults == 0 {
				return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "progress", Type: "function", Function: llm.Function{Name: "SendMessage", Arguments: `{"agent_id":"parent","text":"progress"}`}}}, FinishReason: "tool_calls"}, nil
			}
			started <- first
			select {
			case <-gate:
				return llm.StreamResult{Text: "report-" + first, FinishReason: "stop"}, nil
			case <-ctx.Done():
				return llm.StreamResult{}, ctx.Err()
			}
		}
		if toolResults == 0 {
			return llm.StreamResult{ToolCalls: []llm.ToolCall{
				{ID: "spawn-a", Type: "function", Function: llm.Function{Name: "SpawnAgent", Arguments: `{"task":"child-a"}`}},
				{ID: "spawn-b", Type: "function", Function: llm.Function{Name: "SpawnAgent", Arguments: `{"task":"child-b"}`}},
			}, FinishReason: "tool_calls"}, nil
		}
		parentSteps <- finals
		text := "waiting for remaining reports"
		if finals == 2 {
			text = "both reports received"
		}
		return llm.StreamResult{Text: text, FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	a.services.cfg.MaxTurns = 20
	go a.Run()
	submitChildRuntimeTestInput(t, a, "delegate-input", "parent-task")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("children did not reach reporting boundary")
		}
	}
	select {
	case <-parentSteps:
	case <-ctx.Done():
		t.Fatal("parent never reached implicit wait")
	}
	waitParentPhase(t, a, protocol.PhaseWaitingAgents)
	finish("child-a")
	for {
		select {
		case n := <-parentSteps:
			if n == 1 {
				goto waitingForB
			}
		case <-ctx.Done():
			t.Fatal("first final report did not wake parent")
		}
	}
waitingForB:
	waitParentPhase(t, a, protocol.PhaseWaitingAgents)
	view, err := a.Snapshot(ctx)
	if err != nil || !view.Busy {
		t.Fatalf("parent ended with outstanding work: busy=%v err=%v", view.Busy, err)
	}
	select {
	case n := <-parentSteps:
		t.Fatalf("wait issued another model request with no event: %d", n)
	case <-time.After(50 * time.Millisecond):
	}
	finish("child-b")
	view = waitChildRuntimeTestIdle(t, a)
	if view.Phase != protocol.PhaseIdle {
		t.Fatalf("completed parent retained wait phase: %s", view.Phase)
	}
	records, _, err := a.services.supervisor.readSessionRecords(ctx, protocol.SessionID(a.conversation.sessionID))
	if err != nil {
		t.Fatal(err)
	}
	turns := 0
	for _, record := range records {
		if _, ok := record.Event.(*session.TurnFinished); ok {
			turns++
		}
	}
	if turns != 1 {
		t.Fatalf("completion finished %d parent turns, want 1; last=%+v persistence=%v", turns, view.LastTurn, a.persistenceFailure())
	}
	finals, progress := 0, 0
	for _, m := range view.History {
		if msg, ok := protocol.DecodeCollaboration(m.Content); ok {
			if msg.Kind == "final_result" {
				finals++
			} else {
				progress++
			}
		}
		for _, call := range m.ToolCalls {
			if call.Name == "ReadAgent" || call.Name == "WaitAgent" {
				t.Fatal("test should exercise automatic result delivery")
			}
		}
	}
	if finals != 2 || progress != 2 || a.hasAgentMessage() {
		t.Fatalf("delivery not exactly once: finals=%d progress=%d pending=%v", finals, progress, a.hasAgentMessage())
	}
	rows, _ := a.services.supervisor.ListChildren(ctx)
	for _, row := range rows {
		r, _ := a.services.supervisor.lookup(row.SessionID)
		waitManaged(t, r)
		a.services.supervisor.deliver(r) // retry must not queue another copy or start a turn
	}
	if a.hasAgentMessage() {
		t.Fatal("retry redelivered final result")
	}
}

func waitParentPhase(t *testing.T, a *Agent, phase protocol.RuntimePhase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := a.Watch(ctx, protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for {
		select {
		case update, ok := <-sub.Updates():
			if !ok {
				t.Fatalf("watch closed before %s", phase)
			}
			if update.Snapshot != nil && update.Snapshot.Phase == phase || update.State != nil && update.State.Phase == phase {
				return
			}
		case <-ctx.Done():
			t.Fatalf("parent never entered %s", phase)
		}
	}
}

func TestCancelParentWaitingForDelegatedWork(t *testing.T) {
	p := &childRuntimeTestProvider{name: "cancel-waiting-parent"}
	p.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		hasToolResult := false
		for _, message := range req.Messages {
			if message.Role == "user" && message.Content == "blocked-child" {
				<-ctx.Done()
				return llm.StreamResult{}, ctx.Err()
			}
			hasToolResult = hasToolResult || message.Role == "tool"
		}
		if !hasToolResult {
			return llm.StreamResult{ToolCalls: []llm.ToolCall{{ID: "spawn", Type: "function", Function: llm.Function{
				Name: "SpawnAgent", Arguments: `{"task":"blocked-child"}`,
			}}}, FinishReason: "tool_calls"}, nil
		}
		return llm.StreamResult{Text: "waiting for child", FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, p)
	go a.Run()
	submitChildRuntimeTestInput(t, a, "cancel-wait", "parent-task")
	waitParentPhase(t, a, protocol.PhaseWaitingAgents)
	a.interrupt()
	view := waitChildRuntimeTestIdle(t, a)
	if view.Phase != protocol.PhaseIdle || view.LastTurn.Status != protocol.TurnCancelled {
		t.Fatalf("cancelled wait did not settle: phase=%s outcome=%+v", view.Phase, view.LastTurn)
	}
}

func TestAgentWaitMessageCancellationAndBusyFollowup(t *testing.T) {
	p := &childRuntimeTestProvider{name: "blocked-child", stream: func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		<-ctx.Done()
		return llm.StreamResult{}, ctx.Err()
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	r, err := a.services.supervisor.launch(a, a.lifecycle.rootCtx, childTask{Description: "hold", WaitPolicy: "notify"}, childPurposeTask, "blocked")
	if err != nil {
		t.Fatal(err)
	}
	id := r.fact.Child.SessionID
	if _, err := a.services.supervisor.Control(context.Background(), protocol.AgentControl{ID: "busy-next", SessionID: id, Action: "continue", Text: "next"}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy follow-up was not rejected: %v", err)
	}
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		result := make(chan string, 1)
		go func() {
			reason, _ := a.waitAgentEvent(ctx)
			result <- reason
		}()
		// Arrival may race subscription. Admission must wake both old and new waits.
		msgID := protocol.CommandID(nextRuntimeID("message"))
		if receipt := a.applySubmitInput(protocol.NewSubmitInput(msgID, "", protocol.InputID(msgID), "progress", protocol.InputMessage)); receipt.Rejected() {
			t.Fatal(receipt.Error)
		}
		if reason := <-result; reason != "collaboration" {
			t.Fatalf("lost message wake: %q", reason)
		}
		cancel()
		if err := a.consumeAgentMessages(); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.waitAgentEvent(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait swallowed cancellation: %v", err)
	}
	a.interrupt()
	waitManaged(t, r)
	if r.fact.Child.Run.Status != "cancelled" {
		t.Fatalf("parent interruption left child running: %s", r.fact.Child.Run.Status)
	}
	if _, err := a.services.supervisor.Control(context.Background(), protocol.AgentControl{ID: "stop-idle", SessionID: id, Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	finalCtx, finishWait := context.WithTimeout(context.Background(), 3*time.Second)
	defer finishWait()
	if reason, err := a.waitAgentEvent(finalCtx); err != nil || reason != "collaboration" {
		t.Fatalf("late cancellation result not delivered: %s %v", reason, err)
	}
	if err := a.consumeAgentMessages(); err != nil {
		t.Fatal(err)
	}
	if reason, err := a.waitAgentEvent(context.Background()); err != nil || reason != "no_pending_work" {
		t.Fatalf("consumed events woke again: %s %v", reason, err)
	}
	if a.isBusy() {
		t.Fatal("late final result restarted idle parent")
	}
}

func TestPreviousRunCannotRepublishChildCard(t *testing.T) {
	a := newRuntimeAgent(t)
	id := protocol.SessionID("child-card")
	done := make(chan struct{})
	close(done)
	old := &managedRun{parent: a, done: done, fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: id, Run: protocol.RunView{ID: "previous", Status: "succeeded"}}}}
	current := &managedRun{parent: a, done: done, fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: id, Run: protocol.RunView{ID: "current", Status: "running"}}}}
	s := testSupervisor(a, current)
	sub, err := a.Watch(context.Background(), protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	<-sub.Updates() // initial snapshot
	s.publishChildOutcome(old)
	s.publishChildOutcome(current)
	for {
		select {
		case update := <-sub.Updates():
			if update.Child == nil {
				continue
			}
			if update.Child.Session.Run.ID != "current" {
				t.Fatalf("old delivery replaced live card: %+v", update.Child.Session.Run)
			}
			return
		case <-time.After(time.Second):
			t.Fatal("current child update not published")
		}
	}
}
