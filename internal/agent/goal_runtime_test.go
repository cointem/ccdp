package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func submitGoalTestCommand(t *testing.T, a *Agent, id, action, objective string, budget *int64) protocol.Receipt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receipt, err := a.Submit(ctx, protocol.Command{
		ID: protocol.CommandID(id), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandGoal,
		Goal: &protocol.GoalCommand{Action: action, Objective: objective, TokenBudget: budget},
	})
	if err != nil || receipt.Rejected() {
		t.Fatalf("goal %s: %+v, %v", action, receipt, err)
	}
	return receipt
}

func waitGoalTestState(t *testing.T, a *Agent, status protocol.GoalStatus) protocol.SessionView {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		view, err := a.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !view.Busy && ((status == "" && view.Goal == nil) || (view.Goal != nil && view.Goal.Status == status)) {
			return view
		}
		select {
		case <-ctx.Done():
			t.Fatalf("goal did not settle as %s: goal=%+v, busy=%v, last=%+v", status, view.Goal, view.Busy, view.LastTurn)
		case <-tick.C:
		}
	}
}

func goalCompleteTestResponse() llm.StreamResult {
	return llm.StreamResult{PromptTokens: 10, CachedTokens: 4, CompletionTok: 2, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
		ID: "finish-goal", Type: "function", Function: llm.Function{Name: "UpdateGoal", Arguments: `{"status":"complete","reason":"Implemented the requested result and verified its behavior."}`},
	}}}
}

func TestGoalRuntimeContinuesAcrossTurnsAndCompletes(t *testing.T) {
	var calls atomic.Int32
	provider := &childRuntimeTestProvider{name: "goal-continuation"}
	provider.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		n := calls.Add(1)
		foundGoal := false
		for _, message := range req.Messages {
			if message.Role == "system" {
				text, _ := message.Content.(string)
				foundGoal = foundGoal || strings.Contains(text, "implement-and-verify-goal")
			}
		}
		if !foundGoal {
			return llm.StreamResult{}, fmt.Errorf("request %d lost the current goal", n)
		}
		if n == 3 {
			return goalCompleteTestResponse(), nil
		}
		return llm.StreamResult{Text: "Made progress; further work remains.", PromptTokens: 10, CachedTokens: 4, CompletionTok: 2, FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, provider)
	go a.Run()
	submitGoalTestCommand(t, a, "goal-create", "set", "implement-and-verify-goal", nil)
	view := waitGoalTestState(t, a, protocol.GoalComplete)
	if calls.Load() != 3 || view.Goal.ModelCalls != 3 || view.Goal.TokensUsed != 24 {
		t.Fatalf("unexpected calls/usage: requests=%d goal=%+v", calls.Load(), view.Goal)
	}
	if !strings.Contains(view.Goal.Reason, "verified") || view.Goal.ActiveTimeSeconds <= 0 {
		t.Fatalf("completion evidence/time missing: %+v", view.Goal)
	}
}

func TestGoalRuntimePauseResumeAndClear(t *testing.T) {
	var calls atomic.Int32
	started := make(chan int32, 4)
	provider := &childRuntimeTestProvider{name: "goal-interrupt"}
	provider.stream = func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		n := calls.Add(1)
		started <- n
		if n == 2 {
			return goalCompleteTestResponse(), nil
		}
		<-ctx.Done()
		return llm.StreamResult{}, ctx.Err()
	}
	a, _ := newChildRuntimeTestAgent(t, provider)
	go a.Run()
	submitGoalTestCommand(t, a, "pause-create", "set", "controlled goal", nil)
	awaitGoalProviderStart(t, started, 1)
	submitGoalTestCommand(t, a, "pause-now", "pause", "", nil)
	paused := waitGoalTestState(t, a, protocol.GoalPaused)
	if calls.Load() != 1 {
		t.Fatalf("pause scheduled extra work: %d", calls.Load())
	}
	submitGoalTestCommand(t, a, "resume-now", "resume", "", nil)
	completed := waitGoalTestState(t, a, protocol.GoalComplete)
	if completed.Goal.ID != paused.Goal.ID || calls.Load() != 2 {
		t.Fatalf("resume replaced goal or duplicated work: %+v calls=%d", completed.Goal, calls.Load())
	}
	submitGoalTestCommand(t, a, "clear-create", "set", "another goal", nil)
	awaitGoalProviderStart(t, started, 2)
	awaitGoalProviderStart(t, started, 3)
	submitGoalTestCommand(t, a, "clear-now", "clear", "", nil)
	waitGoalTestState(t, a, "")
	if calls.Load() != 3 {
		t.Fatalf("clear scheduled extra work: %d", calls.Load())
	}
}

func awaitGoalProviderStart(t *testing.T, started <-chan int32, want int32) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("provider call = %d, want %d", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("provider call %d never started", want)
	}
}

func TestGoalRuntimeBudgetAndCallLimitsSpanContinuations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budget   int64
		maxCalls int
	}{
		{name: "token-budget", budget: 15, maxCalls: 20},
		{name: "model-calls", maxCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			provider := &childRuntimeTestProvider{name: "goal-limit"}
			provider.stream = func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				calls.Add(1)
				return llm.StreamResult{Text: "Further work remains.", PromptTokens: 10, CachedTokens: 4, CompletionTok: 2, FinishReason: "stop"}, nil
			}
			a, _ := newChildRuntimeTestAgent(t, provider)
			a.services.cfg.MaxTurns = tc.maxCalls
			go a.Run()
			var budget *int64
			if tc.budget > 0 {
				budget = &tc.budget
			}
			submitGoalTestCommand(t, a, "limited-create", "set", "keep working", budget)
			view := waitGoalTestState(t, a, protocol.GoalBudgetLimited)
			if calls.Load() != 2 || view.Goal.ModelCalls != 2 || view.Goal.TokensUsed != 16 {
				t.Fatalf("limit did not span turns: calls=%d goal=%+v", calls.Load(), view.Goal)
			}
		})
	}
}

func TestGoalRuntimeQueuedUserInputPrecedesAutomaticContinuation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan int32, 1)
	release := make(chan struct{})
	provider := &childRuntimeTestProvider{name: "goal-user-priority"}
	provider.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		n := calls.Add(1)
		if n == 1 {
			started <- n
			select {
			case <-release:
			case <-ctx.Done():
				return llm.StreamResult{}, ctx.Err()
			}
			return llm.StreamResult{Text: "Progress made.", PromptTokens: 10, CompletionTok: 2, FinishReason: "stop"}, nil
		}
		lastUser := ""
		for _, message := range req.Messages {
			if message.Role == "user" {
				lastUser, _ = message.Content.(string)
			}
		}
		if !strings.Contains(lastUser, "prioritize-this-user-message") {
			return llm.StreamResult{}, fmt.Errorf("automatic continuation overtook user input: %q", lastUser)
		}
		return goalCompleteTestResponse(), nil
	}
	a, _ := newChildRuntimeTestAgent(t, provider)
	go a.Run()
	submitGoalTestCommand(t, a, "priority-create", "set", "goal work", nil)
	awaitGoalProviderStart(t, started, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receipt, err := a.Submit(ctx, protocol.NewSubmitInput("user-followup", protocol.SessionID(a.SessionID()), "user-followup", "prioritize-this-user-message", protocol.InputFollowup))
	if err != nil || receipt.Rejected() {
		t.Fatalf("queue user input: %+v %v", receipt, err)
	}
	close(release)
	waitGoalTestState(t, a, protocol.GoalComplete)
	if calls.Load() != 2 {
		t.Fatalf("expected one user turn before any automatic continuation, calls=%d", calls.Load())
	}
}

func TestGoalRuntimeBlocksAfterThreeEmptyResponses(t *testing.T) {
	var calls atomic.Int32
	provider := &childRuntimeTestProvider{name: "goal-empty"}
	provider.stream = func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		calls.Add(1)
		return llm.StreamResult{PromptTokens: 5, FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, provider)
	go a.Run()
	submitGoalTestCommand(t, a, "empty-create", "set", "finish actual work", nil)
	view := waitGoalTestState(t, a, protocol.GoalBlocked)
	if calls.Load() != 3 || !strings.Contains(view.Goal.Reason, "empty") {
		t.Fatalf("empty responses did not stop: calls=%d goal=%+v", calls.Load(), view.Goal)
	}
}

func TestGoalRuntimeStoppedBatchPreservesCancelledToolResults(t *testing.T) {
	for _, budgetStop := range []bool{false, true} {
		t.Run(fmt.Sprintf("budget-%t", budgetStop), func(t *testing.T) {
			provider := &childRuntimeTestProvider{name: "goal-batch"}
			provider.stream = func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				response := goalCompleteTestResponse()
				if budgetStop {
					response.ToolCalls = nil
				}
				response.ToolCalls = append(response.ToolCalls, llm.ToolCall{ID: "after-stop", Type: "function", Function: llm.Function{Name: "FormalAuditEffect", Arguments: `{}`}})
				return response, nil
			}
			a, _ := newChildRuntimeTestAgent(t, provider)
			effect := &formalAuditEffect{t: t, a: a}
			a.services.registry.Register(effect)
			go a.Run()
			var budget *int64
			wantStatus := protocol.GoalComplete
			if budgetStop {
				n := int64(1)
				budget = &n
				wantStatus = protocol.GoalBudgetLimited
			}
			submitGoalTestCommand(t, a, "batch-create", "set", "complete before any additional effect", budget)
			waitGoalTestState(t, a, wantStatus)
			if effect.count.Load() != 0 {
				t.Fatal("tool after goal stop was executed")
			}
			paired := false
			for _, message := range a.History() {
				if message.ToolCallID == "after-stop" {
					paired = strings.HasPrefix(message.Content, "Error: ") && strings.Contains(message.Content, "goal is stopped")
				}
			}
			if !paired {
				t.Fatal("cancelled tool call has no paired error result")
			}
			records, err := a.persistenceHandle().Read(session.Beginning)
			if err != nil {
				t.Fatal(err)
			}
			finished := false
			for _, record := range records {
				switch event := record.Event.(type) {
				case *session.ToolStarted:
					if event.Call.CallID == "after-stop" {
						t.Fatal("cancelled tool was durably marked started")
					}
				case *session.ToolFinished:
					if event.CallID == "after-stop" {
						finished = event.Status == "cancelled" && event.SideEffect == "none"
					}
				}
			}
			if !finished {
				t.Fatal("cancelled tool has no durable cancelled completion")
			}
		})
	}
}

func TestGoalRuntimeAccountsChildWorkWithoutGrantingGoalControl(t *testing.T) {
	var calls atomic.Int32
	provider := &childRuntimeTestProvider{name: "goal-child-accounting"}
	provider.stream = func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		calls.Add(1)
		child, hasResult := false, false
		getResult, updateResult := "", ""
		for _, message := range req.Messages {
			text, _ := message.Content.(string)
			if message.Role == "user" && text == "goal-child-assignment" {
				child = true
			}
			if message.Role == "tool" {
				hasResult = true
				if message.ToolCallID == "child-get-goal" {
					getResult = text
				}
				if message.ToolCallID == "child-update-goal" {
					updateResult = text
				}
			}
		}
		if child {
			if !hasResult {
				return llm.StreamResult{PromptTokens: 10, CachedTokens: 4, CompletionTok: 2, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
					{ID: "child-get-goal", Type: "function", Function: llm.Function{Name: "GetGoal", Arguments: `{}`}},
					{ID: "child-update-goal", Type: "function", Function: llm.Function{Name: "UpdateGoal", Arguments: `{"status":"complete","reason":"I only completed the child assignment."}`}},
				}}, nil
			}
			if !strings.Contains(getResult, "parent-accounting-goal") || !strings.Contains(updateResult, "cannot update goals") {
				return llm.StreamResult{}, fmt.Errorf("incorrect child authority: get=%q update=%q", getResult, updateResult)
			}
			return llm.StreamResult{Text: "Child assignment verified.", PromptTokens: 10, CachedTokens: 4, CompletionTok: 2, FinishReason: "stop"}, nil
		}
		if !hasResult {
			return llm.StreamResult{PromptTokens: 10, CachedTokens: 4, CompletionTok: 2, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
				ID: "goal-join", Type: "function", Function: llm.Function{Name: "JoinChildTest", Arguments: `{"description":"goal-child-assignment"}`},
			}}}, nil
		}
		return goalCompleteTestResponse(), nil
	}
	a, cfg := newChildRuntimeTestAgent(t, provider)
	a.services.cfg.MaxTurns = 20
	go a.Run()
	submitGoalTestCommand(t, a, "child-accounting-create", "set", "parent-accounting-goal", nil)
	view := waitGoalTestState(t, a, protocol.GoalComplete)
	if calls.Load() != 4 || view.Goal.ModelCalls != 4 || view.Goal.TokensUsed != 32 {
		t.Fatalf("child usage missing or duplicated: calls=%d goal=%+v", calls.Load(), view.Goal)
	}
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(cfg.SessionDir, a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Goal == nil || snapshot.Goal.TokensUsed != 32 || snapshot.Goal.ModelCalls != 4 {
		t.Fatalf("saved child usage differs from runtime: %+v", snapshot.Goal)
	}
}

func TestGoalRuntimeEditWaitsForOldStepAndRequiresNewCompletion(t *testing.T) {
	var calls atomic.Int32
	started := make(chan int32, 1)
	release := make(chan struct{})
	provider := &childRuntimeTestProvider{name: "goal-edit-boundary"}
	provider.stream = func(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		n := calls.Add(1)
		if n == 1 {
			started <- n
			select {
			case <-release:
			case <-ctx.Done():
				return llm.StreamResult{}, ctx.Err()
			}
			response := goalCompleteTestResponse()
			response.ToolCalls[0].Function.Arguments = `{"status":"complete","reason":"Verified the old objective only."}`
			return response, nil
		}
		foundNewGoal := false
		for _, message := range req.Messages {
			if message.Role == "system" {
				text, _ := message.Content.(string)
				foundNewGoal = foundNewGoal || strings.Contains(text, "new-edited-objective")
			}
		}
		if n != 2 || !foundNewGoal {
			return llm.StreamResult{}, fmt.Errorf("edited goal was not applied to next request: call=%d present=%v", n, foundNewGoal)
		}
		response := goalCompleteTestResponse()
		response.ToolCalls[0].ID = "finish-edited-goal"
		response.ToolCalls[0].Function.Arguments = `{"status":"complete","reason":"Verified the new edited objective."}`
		return response, nil
	}
	a, _ := newChildRuntimeTestAgent(t, provider)
	go a.Run()
	submitGoalTestCommand(t, a, "edit-create", "set", "old-original-objective", nil)
	awaitGoalProviderStart(t, started, 1)
	submitGoalTestCommand(t, a, "edit-in-flight", "edit", "new-edited-objective", nil)
	staged, err := a.Snapshot(context.Background())
	if err != nil || staged.Goal == nil || staged.Goal.Objective != "old-original-objective" || staged.Goal.PendingObjective != "new-edited-objective" {
		t.Fatalf("edit did not wait for the old step: goal=%+v err=%v", staged.Goal, err)
	}
	close(release)
	view := waitGoalTestState(t, a, protocol.GoalComplete)
	if calls.Load() != 2 || view.Goal.Objective != "new-edited-objective" || view.Goal.PendingObjective != "" || !strings.Contains(view.Goal.Reason, "new edited objective") {
		t.Fatalf("old completion ended edited goal: calls=%d goal=%+v", calls.Load(), view.Goal)
	}
	if view.Goal.ID != staged.Goal.ID || view.Goal.TokensUsed != 16 || view.Goal.ModelCalls != 2 {
		t.Fatalf("edit replaced goal or reset accounting: %+v", view.Goal)
	}
}

func TestGoalRuntimePausedGoalDoesNotAccountOrdinaryConversation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan int32, 1)
	provider := &childRuntimeTestProvider{name: "goal-paused-conversation"}
	provider.stream = func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		if calls.Add(1) == 1 {
			started <- 1
			<-ctx.Done()
			return llm.StreamResult{PromptTokens: 5}, ctx.Err()
		}
		return llm.StreamResult{Text: "ordinary-conversation-answer", PromptTokens: 100, CompletionTok: 50, FinishReason: "stop"}, nil
	}
	a, _ := newChildRuntimeTestAgent(t, provider)
	go a.Run()
	submitGoalTestCommand(t, a, "ordinary-create", "set", "paused objective", nil)
	awaitGoalProviderStart(t, started, 1)
	submitGoalTestCommand(t, a, "ordinary-pause", "pause", "", nil)
	paused := waitGoalTestState(t, a, protocol.GoalPaused)
	submitChildRuntimeTestInput(t, a, "ordinary-input", "Answer an unrelated short question.")
	view := waitGoalTestState(t, a, protocol.GoalPaused)
	if calls.Load() != 2 {
		t.Fatalf("ordinary conversation failed or restarted the goal: calls=%d", calls.Load())
	}
	if view.Goal.TokensUsed != paused.Goal.TokensUsed || view.Goal.ModelCalls != paused.Goal.ModelCalls || view.Goal.ActiveTimeSeconds != paused.Goal.ActiveTimeSeconds {
		t.Fatalf("ordinary conversation charged to paused goal: before=%+v after=%+v", paused.Goal, view.Goal)
	}
	if len(view.History) == 0 || view.History[len(view.History)-1].Content != "ordinary-conversation-answer" {
		t.Fatalf("ordinary conversation did not complete: %+v", view.LastTurn)
	}
}
