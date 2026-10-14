package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

type headlessChildDirectory struct {
	protocol.SessionDirectory
	rows   []protocol.ChildSession
	owners map[protocol.SessionID]*headlessTestClient
}

func (d *headlessChildDirectory) ListChildren(context.Context) ([]protocol.ChildSession, error) {
	return d.rows, nil
}
func (d *headlessChildDirectory) OpenReader(_ context.Context, id protocol.SessionID) (protocol.SessionClient, error) {
	return d.owners[id], nil
}

type headlessDirectoryClient struct {
	headlessTestClient
	directory *headlessChildDirectory
}

func (c *headlessDirectoryClient) Sessions() protocol.SessionDirectory { return c.directory }

func TestHeadlessChildInteractionsRouteToOwnerAndRecoverFromDirectory(t *testing.T) {
	for _, kind := range []string{"question", "plan"} {
		t.Run(kind, func(t *testing.T) {
			child := protocol.ChildSession{SessionID: "child", Run: protocol.RunView{Status: "waiting_" + kind}}
			if kind == "question" {
				child.Question = &protocol.QuestionRequest{ID: "pending"}
			} else {
				child.Plan = &protocol.PlanView{ID: "pending", Version: 3}
			}
			owner := &headlessTestClient{}
			root := &headlessDirectoryClient{directory: &headlessChildDirectory{rows: []protocol.ChildSession{child}, owners: map[protocol.SessionID]*headlessTestClient{"child": owner}}}
			w := &headlessWatch{}
			if err := w.childSnapshotDecisions(context.Background(), root, outJSON); err != nil {
				t.Fatal(err)
			}
			if err := w.childDecision(context.Background(), root, child, outJSON); err != nil {
				t.Fatal(err)
			}
			if len(root.commands) != 0 || len(owner.commands) != 1 || owner.commands[0].SessionID != "child" {
				t.Fatalf("decision ownership/dedup: root=%+v child=%+v", root.commands, owner.commands)
			}
			cmd := owner.commands[0]
			if kind == "question" && (cmd.Answer == nil || !cmd.Answer.Cancelled) {
				t.Fatalf("question: %+v", cmd)
			}
			if kind == "plan" && (cmd.Plan == nil || cmd.Plan.Approve || cmd.Plan.PlanVersion != 3) {
				t.Fatalf("plan: %+v", cmd)
			}
		})
	}
}

func TestHeadlessFailedOwnerSnapshotTerminatesWithActiveChild(t *testing.T) {
	for _, resync := range []bool{false, true} {
		t.Run(map[bool]string{false: "snapshot", true: "resync"}[resync], func(t *testing.T) {
			client := &headlessTestClient{snapshot: protocol.SessionView{SessionID: "root", OutstandingWork: 1, LastTurn: &protocol.TurnOutcome{TurnID: "failed", Status: protocol.TurnFailed, Error: "original provider failure"}}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			w := &headlessWatch{client: client}
			if resync {
				old := &headlessTestSubscription{ch: make(chan protocol.Update, 1)}
				old.send(protocol.Update{Type: protocol.UpdateResyncRequired})
				w.sub = old
			} else if err := w.open(ctx); err != nil {
				t.Fatal(err)
			}
			defer w.close()
			err := consumeHeadlessTurn(ctx, ctx, w, "root", outJSON, &turnSink{}, 0, client)
			if err == nil || !strings.Contains(err.Error(), "original provider failure") {
				t.Fatalf("owner error replaced: %v", err)
			}
			if len(client.commands) != 1 || client.commands[0].Type != protocol.CommandStop {
				t.Fatalf("cleanup commands: %+v", client.commands)
			}
		})
	}
}
