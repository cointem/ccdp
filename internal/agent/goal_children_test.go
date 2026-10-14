package agent

import (
	"context"
	"testing"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestGoalChildrenIncludeDescendantsAndAllWaitPolicies(t *testing.T) {
	owner, otherOwner := &Agent{}, &Agent{}
	owner.conversation.durable.Goal = &protocol.Goal{ID: "goal"}
	owner.services.supervisor = testSupervisor(owner)
	type fixture struct {
		ctx           context.Context
		run           *managedRun
		wantCancelled bool
	}
	rows := map[string]fixture{}
	for _, input := range []struct {
		name, goal, status, policy string
		owned                      bool
		want                       bool
	}{
		{"join-grandchild", "goal", "running", "join", true, true},
		{"notify-child", "goal", "waiting_approval", "notify", true, true},
		{"observe-grandchild", "goal", "queued", "observe", true, true},
		{"different-goal", "other", "running", "join", true, false},
		{"different-owner", "goal", "running", "join", false, false},
		{"ordinary", "", "running", "notify", true, false},
		{"finished-parent", "goal", "succeeded", "join", true, false},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runOwner := owner
		if !input.owned {
			runOwner = otherOwner
		}
		run := &managedRun{parent: &Agent{}, opts: Options{goalOwner: runOwner, goalOwnerID: input.goal}, cancel: cancel, fact: session.ChildRunRecorded{Child: protocol.ChildSession{ParentSessionID: "intermediate", Run: protocol.RunView{ID: protocol.RunID(input.name), Status: input.status, WaitPolicy: input.policy}}}}
		owner.services.supervisor.directory.Restore(run.fact.Child, run, true)
		rows[input.name] = fixture{ctx, run, input.want}
	}
	if !owner.goalChildrenActive() {
		t.Fatal("active descendants were ignored")
	}
	owner.cancelGoalChildren("goal")
	for name, row := range rows {
		if (row.ctx.Err() != nil) != row.wantCancelled {
			t.Fatalf("%s canceled=%v want=%v", name, row.ctx.Err(), row.wantCancelled)
		}
		if row.wantCancelled {
			row.run.mu.Lock()
			row.run.fact.Child.Run.Status = "cancelled"
			row.run.mu.Unlock()
		}
	}
	if owner.goalChildrenActive() {
		t.Fatal("unrelated active runs blocked completion")
	}
}

func TestGoalClearCancelsCapturedIDWithoutCurrentGoal(t *testing.T) {
	owner := &Agent{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &managedRun{opts: Options{goalOwner: owner, goalOwnerID: "cleared"}, cancel: cancel, fact: session.ChildRunRecorded{Child: protocol.ChildSession{Run: protocol.RunView{Status: "running"}}}}
	owner.services.supervisor = testSupervisor(owner, run)
	owner.cancelGoalChildren("cleared")
	if ctx.Err() == nil {
		t.Fatal("clear failed to cancel captured goal id")
	}
	if owner.goalChildrenActive() {
		t.Fatal("cleared goal still reported active")
	}
}
