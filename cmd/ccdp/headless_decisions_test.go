package main

import (
	"ccdp/internal/protocol"
	"context"
	"errors"
	"testing"
	"time"
)

type recoveryDecisionClient struct{ headlessTestClient }

func (c *recoveryDecisionClient) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	receipt, err := c.headlessTestClient.Submit(ctx, cmd)
	if err != nil {
		return receipt, err
	}
	var event protocol.EventView
	switch cmd.Type {
	case protocol.CommandApproveTool:
		event = protocol.EventView{Kind: protocol.EventApprovalRequest, Approval: c.snapshot.Approval}
	case protocol.CommandApprovePlan:
		event = protocol.EventView{Kind: protocol.EventPlanReady, Plan: c.snapshot.Plan}
	case protocol.CommandAnswerQuestion:
		event = protocol.EventView{Kind: protocol.EventQuestionRequest, Question: c.snapshot.Question}
	default:
		return receipt, err
	}
	// A delayed event can follow the snapshot that already settled it.
	c.publish(protocol.Update{Type: protocol.UpdateStream, Event: &event})
	c.publish(protocol.Update{Type: protocol.UpdateStream, Event: &protocol.EventView{Kind: protocol.EventTurnDone}})
	return receipt, nil
}

func TestHeadlessDecisionsRecoverFromSnapshot(t *testing.T) {
	cases := []struct {
		name string
		view protocol.SessionView
		want protocol.CommandType
	}{
		{"approval", protocol.SessionView{Approval: &protocol.ApprovalView{ID: "approval"}}, protocol.CommandApproveTool},
		{"plan", protocol.SessionView{Plan: &protocol.PlanView{ID: "plan", Version: 3}}, protocol.CommandApprovePlan},
		{"question", protocol.SessionView{Question: &protocol.QuestionRequest{ID: "question"}}, protocol.CommandAnswerQuestion},
	}
	for _, tc := range cases {
		for _, resync := range []bool{false, true} {
			name := tc.name + "/snapshot"
			if resync {
				name = tc.name + "/resync"
			}
			t.Run(name, func(t *testing.T) {
				c := &recoveryDecisionClient{}
				c.snapshot = tc.view
				c.snapshot.SessionID = "s"
				c.snapshot.Busy = true
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				w := &headlessWatch{client: c}
				if resync {
					old := &headlessTestSubscription{ch: make(chan protocol.Update, 1)}
					old.send(protocol.Update{Type: protocol.UpdateResyncRequired})
					w.sub = old
				} else if err := w.open(ctx); err != nil {
					t.Fatal(err)
				}
				defer w.close()
				if err := consumeHeadlessTurn(ctx, ctx, w, "s", outJSON, &turnSink{}, 0, c); err != nil {
					t.Fatal(err)
				}
				if len(c.commands) != 1 || c.commands[0].Type != tc.want {
					t.Fatalf("commands: %+v", c.commands)
				}
				cmd := c.commands[0]
				if cmd.Approval != nil && cmd.Approval.Approve {
					t.Fatal("headless approved tool")
				}
				if cmd.Plan != nil && (cmd.Plan.Approve || cmd.Plan.PlanVersion != 3) {
					t.Fatalf("plan: %+v", cmd.Plan)
				}
				if cmd.Answer != nil && !cmd.Answer.Cancelled {
					t.Fatal("headless answered question")
				}
			})
		}
	}
}

func TestHeadlessDecisionsPlanVersionAndFailure(t *testing.T) {
	c := &headlessTestClient{}
	d := headlessDecisions{client: c, sessionID: "s", handled: map[headlessDecisionKey]bool{}}
	for _, version := range []uint64{1, 1, 2} {
		if err := d.snapshot(context.Background(), protocol.SessionView{Plan: &protocol.PlanView{ID: "p", Version: version}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.commands) != 2 {
		t.Fatalf("plan commands=%d", len(c.commands))
	}
	c.transportErr = errors.New("transport down")
	err := d.snapshot(context.Background(), protocol.SessionView{Approval: &protocol.ApprovalView{ID: "a"}})
	if !errors.Is(err, c.transportErr) {
		t.Fatalf("error=%v", err)
	}
	if d.handled[headlessDecisionKey{kind: protocol.CommandApproveTool, id: "a"}] {
		t.Fatal("failed decision marked handled")
	}
}

type initialDecisionClient struct {
	headlessTestClient
	settled bool
}

func (c *initialDecisionClient) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	if cmd.Type == protocol.CommandSubmitInput && !c.settled {
		return protocol.Receipt{Status: protocol.ReceiptRejected, Error: &protocol.CommandError{Code: protocol.ErrorBusy, Message: "initial decision not settled"}}, nil
	}
	if cmd.Type == protocol.CommandApproveTool || cmd.Type == protocol.CommandApprovePlan || cmd.Type == protocol.CommandAnswerQuestion {
		c.settled = true
	}
	return c.headlessTestClient.Submit(ctx, cmd)
}
func TestRunHeadlessSettlesInitialDecisionBeforeInput(t *testing.T) {
	for _, kind := range []string{"approval", "plan", "question"} {
		t.Run(kind, func(t *testing.T) {
			c := &initialDecisionClient{}
			c.snapshot = headlessTestSnapshot()
			switch kind {
			case "approval":
				c.snapshot.Approval = &protocol.ApprovalView{ID: "a"}
			case "plan":
				c.snapshot.Plan = &protocol.PlanView{ID: "p", Version: 1}
			case "question":
				c.snapshot.Question = &protocol.QuestionRequest{ID: "q"}
			}
			out, code := captureHeadlessStdout(t, func() int { return runHeadlessClient(c, []string{"continue"}, outJSON, 2) })
			if code != 0 || !c.settled {
				t.Fatalf("initial decision not handled: code=%d output=%s", code, out)
			}
		})
	}
}
