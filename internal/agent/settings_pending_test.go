package agent

import (
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"testing"
)

// TestGenerationSettingsApplyWhileBusy covers the reasoning effort/verbosity
// dimension: a change submitted mid-turn applies to live config immediately and
// stages nothing, because the running step already froze its own generation.
func TestGenerationSettingsApplyWhileBusy(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.mu.Lock()
	ag.busy = true
	ag.mu.Unlock()

	effort, verbosity := "max", "low"
	receipt := ag.applyCommand(protocol.Command{
		ID: "busy-generation", SessionID: protocol.SessionID(ag.SessionID()),
		Type:       protocol.CommandSetGeneration,
		Generation: &protocol.SetGeneration{ReasoningEffort: &effort, Verbosity: &verbosity},
	})
	if receipt.Rejected() || receipt.Status != protocol.ReceiptApplied {
		t.Fatalf("generation while busy = %+v, want applied", receipt)
	}
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.cfg.ReasoningEffort != "max" || ag.cfg.Verbosity != "low" {
		t.Fatalf("generation did not apply immediately: effort=%q verbosity=%q", ag.cfg.ReasoningEffort, ag.cfg.Verbosity)
	}
}

// TestPermissionAppliesImmediatelyWhileBusy covers the discrete-decision lane:
// each permission change mid-turn applies to live state at once, and a second
// change simply overwrites the first rather than staging a supersession candidate.
func TestPermissionAppliesImmediatelyWhileBusy(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.mu.Lock()
	ag.busy = true
	ag.mu.Unlock()
	sid := protocol.SessionID(ag.SessionID())

	first := ag.applyCommand(protocol.Command{ID: "perm-accept", SessionID: sid,
		Type:             protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeAcceptEdits)}}})
	if first.Rejected() || first.Status != protocol.ReceiptApplied {
		t.Fatalf("first permission change = %+v, want applied", first)
	}
	second := ag.applyCommand(protocol.Command{ID: "perm-bypass", SessionID: sid,
		Type:             protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeBypass)}}})
	if second.Rejected() || second.Status != protocol.ReceiptApplied {
		t.Fatalf("second permission change = %+v, want applied", second)
	}

	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.cfg.PermissionMode != string(permissions.ModeBypass) {
		t.Fatalf("live mode did not track the last change: %s", ag.cfg.PermissionMode)
	}
	if ag.perms.CurrentMode() != permissions.ModeBypass {
		t.Fatalf("permission manager not synced to live mode: %s", ag.perms.CurrentMode())
	}
}

func TestPermissionIdleApply(t *testing.T) {
	ag := newRuntimeAgent(t)
	sid := protocol.SessionID(ag.SessionID())
	receipt := ag.applyCommand(protocol.Command{ID: "perm-accept-idle", SessionID: sid,
		Type:             protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeAcceptEdits)}}})
	if receipt.Rejected() || receipt.Status != protocol.ReceiptApplied {
		t.Fatalf("idle permission change = %+v, want applied", receipt)
	}
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.cfg.PermissionMode != string(permissions.ModeAcceptEdits) {
		t.Fatalf("idle mode did not apply immediately: %s", ag.cfg.PermissionMode)
	}
	if ag.perms.CurrentMode() != permissions.ModeAcceptEdits {
		t.Fatalf("perms manager did not update: %s", ag.perms.CurrentMode())
	}
}

func TestSettingsPreservePendingPlanWorkflow(t *testing.T) {
	effort := "high"
	for _, cmd := range []protocol.Command{
		{ID: "generation", Type: protocol.CommandSetGeneration, Generation: &protocol.SetGeneration{ReasoningEffort: &effort}},
		{ID: "permission", Type: protocol.CommandSetPermissionPolicy, PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeAcceptEdits)}}},
		{ID: "plan_noop", Type: protocol.CommandSetExecutionMode, ExecutionMode: &protocol.SetExecutionMode{Mode: protocol.ExecutionModePlan}},
	} {
		t.Run(string(cmd.ID), func(t *testing.T) {
			a := newRuntimeAgent(t)
			if err := a.setExecutionMode(true); err != nil {
				t.Fatal(err)
			}
			pending := &PlanRequest{ID: "pending-plan", Plan: "Review this plan"}
			resp := make(chan bool, 1)
			a.mu.Lock()
			a.workflow = protocol.WorkflowAwaitingDecision
			a.pendingPlan = pending
			a.planResp = resp
			a.mu.Unlock()
			cmd.SessionID = protocol.SessionID(a.SessionID())
			if r := a.applyCommand(cmd); r.Rejected() {
				t.Fatal(r)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.workflow != protocol.WorkflowAwaitingDecision || a.pendingPlan != pending || a.planResp != resp || !a.planMode {
				t.Fatalf("pending approval changed: workflow=%s plan=%+v", a.workflow, a.pendingPlan)
			}
		})
	}
}
