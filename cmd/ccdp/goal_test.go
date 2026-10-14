package main

import (
	"context"
	"strings"
	"testing"

	"ccdp/internal/protocol"
)

func TestGoalFlags(t *testing.T) {
	tests := []struct {
		name, objective, session, prompt, input string
		budget                                  int64
		budgetSet, resume, picker, bad          bool
	}{
		{name: "create", objective: "ship", input: "text"},
		{name: "budget", objective: "ship", input: "text", budget: 400, budgetSet: true},
		{name: "resume", session: "saved", resume: true, input: "text"},
		{name: "no session", resume: true, input: "text", bad: true},
		{name: "conflict", objective: "ship", session: "saved", resume: true, input: "text", bad: true},
		{name: "negative", objective: "ship", input: "text", budget: -1, budgetSet: true, bad: true},
		{name: "zero", objective: "ship", input: "text", budgetSet: true, bad: true},
		{name: "budget alone", input: "text", budget: 50, budgetSet: true, bad: true},
		{name: "prompt", objective: "ship", input: "text", prompt: "other", bad: true},
		{name: "stream", objective: "ship", input: "stream-json", bad: true},
		{name: "picker", objective: "ship", input: "text", picker: true, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := headlessGoalCommand(tt.objective, tt.budget, tt.budgetSet, tt.resume, tt.session, tt.prompt, tt.input, tt.picker)
			if (err != nil) != tt.bad {
				t.Fatal(err)
			}
		})
	}
}

type goalHeadlessClient struct {
	headlessTestClient
	status protocol.GoalStatus
}

func (c *goalHeadlessClient) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	if cmd.Type != protocol.CommandGoal {
		return c.headlessTestClient.Submit(ctx, cmd)
	}
	c.mu.Lock()
	c.commands = append(c.commands, cmd)
	c.mu.Unlock()
	active := &protocol.Goal{ID: "g", Objective: "finish", Status: protocol.GoalActive}
	c.publish(protocol.Update{Event: &protocol.EventView{Kind: protocol.EventGoalChanged, Goal: active}})
	c.publish(protocol.Update{Event: &protocol.EventView{Kind: protocol.EventStream, Text: "first turn; "}})
	c.publish(protocol.Update{Event: &protocol.EventView{Kind: protocol.EventTurnDone}})
	stopped := *active
	stopped.Status = c.status
	stopped.Reason = "verified"
	stopped.TokensUsed = 300
	c.publish(protocol.Update{Event: &protocol.EventView{Kind: protocol.EventGoalChanged, Goal: &stopped}})
	c.publish(protocol.Update{Event: &protocol.EventView{Kind: protocol.EventStream, Text: "final answer"}})
	c.publish(protocol.Update{Event: &protocol.EventView{Kind: protocol.EventTurnDone}})
	return protocol.Receipt{CommandID: cmd.ID, Status: protocol.ReceiptApplied}, nil
}

func TestHeadlessGoalWaitsAcrossTurnsAndReportsResult(t *testing.T) {
	for _, status := range []protocol.GoalStatus{protocol.GoalComplete, protocol.GoalBlocked, protocol.GoalBudgetLimited, protocol.GoalPaused} {
		for _, format := range []outputFormat{outText, outJSON, outStreamJSON} {
			t.Run(string(status)+"/"+string(format), func(t *testing.T) {
				c := &goalHeadlessClient{status: status}
				c.snapshot = protocol.SessionView{SessionID: "root"}
				out, code := captureHeadlessStdout(t, func() int {
					return runHeadlessClientWithGoal(c, nil, format, 2, &protocol.GoalCommand{Action: "set", Objective: "finish"})
				})
				if (code == 0) != (status == protocol.GoalComplete) || !strings.Contains(out, "final answer") {
					t.Fatalf("code=%d output=%s", code, out)
				}
				if format != outText && (!strings.Contains(out, `"goal"`) || !strings.Contains(out, string(status))) {
					t.Fatal(out)
				}
				if len(c.commands) != 1 || c.commands[0].Type != protocol.CommandGoal {
					t.Fatalf("commands=%+v", c.commands)
				}
			})
		}
	}
}

func TestGoalHeadlessSnapshotRecoveryAndRuntimeFailure(t *testing.T) {
	t.Run("snapshot", func(t *testing.T) {
		c := &headlessTestClient{}
		subscription := &headlessTestSubscription{ch: make(chan protocol.Update, 2)}
		watch := &headlessWatch{client: c, sub: subscription, goalRun: true}
		subscription.send(protocol.Update{Snapshot: &protocol.SessionView{SessionID: "root", Goal: &protocol.Goal{ID: "g", Status: protocol.GoalComplete}, History: []protocol.MessageView{{Role: "assistant", Content: "recovered final"}}}})
		sink := &turnSink{}
		if err := consumeHeadlessTurn(context.Background(), context.Background(), watch, "root", outJSON, sink, 0, c); err != nil {
			t.Fatal(err)
		}
		if sink.text.String() != "recovered final" {
			t.Fatal(sink.text.String())
		}
	})
	t.Run("rejected creation", func(t *testing.T) {
		c := &headlessTestClient{reject: true, snapshot: protocol.SessionView{SessionID: "root"}}
		_, code := captureHeadlessStdout(t, func() int {
			return runHeadlessClientWithGoal(c, nil, outJSON, 2, &protocol.GoalCommand{Action: "set", Objective: "goal"})
		})
		if code != 1 {
			t.Fatalf("rejected command exit=%d", code)
		}
	})
}
