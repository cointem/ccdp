package agent

import (
	"testing"

	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

// TestGenerationSettingsApplyWhileBusy covers the reasoning effort/verbosity
// dimension: a change submitted mid-turn applies to live config immediately and
// stages nothing, because the running step already froze its own generation.
func TestGenerationSettingsApplyWhileBusy(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.mu.Lock()
	ag.turn.busy = true
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
	if ag.services.cfg.ReasoningEffort != "max" || ag.services.cfg.Verbosity != "low" {
		t.Fatalf("generation did not apply immediately: effort=%q verbosity=%q", ag.services.cfg.ReasoningEffort, ag.services.cfg.Verbosity)
	}
}

// TestPermissionAppliesImmediatelyWhileBusy covers the discrete-decision lane:
// each permission change mid-turn applies to live state at once, and a second
// change simply overwrites the first rather than staging a supersession candidate.
func TestPermissionAppliesImmediatelyWhileBusy(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.mu.Lock()
	ag.turn.busy = true
	ag.mu.Unlock()
	sid := protocol.SessionID(ag.SessionID())

	first := ag.applyCommand(protocol.Command{ID: "perm-accept", SessionID: sid,
		Type:             protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeManual)}}})
	if first.Rejected() || first.Status != protocol.ReceiptApplied {
		t.Fatalf("first permission change = %+v, want applied", first)
	}
	second := ag.applyCommand(protocol.Command{ID: "perm-auto", SessionID: sid,
		Type:             protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeAuto)}}})
	if second.Rejected() || second.Status != protocol.ReceiptApplied {
		t.Fatalf("second permission change = %+v, want applied", second)
	}

	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.services.cfg.PermissionMode != string(permissions.ModeAuto) {
		t.Fatalf("live mode did not track the last change: %s", ag.services.cfg.PermissionMode)
	}
	if ag.services.perms.CurrentMode() != permissions.ModeAuto {
		t.Fatalf("permission manager not synced to live mode: %s", ag.services.perms.CurrentMode())
	}
}

func TestPermissionIdleApply(t *testing.T) {
	ag := newRuntimeAgent(t)
	sid := protocol.SessionID(ag.SessionID())
	receipt := ag.applyCommand(protocol.Command{ID: "perm-accept-idle", SessionID: sid,
		Type:             protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeManual)}}})
	if receipt.Rejected() || receipt.Status != protocol.ReceiptApplied {
		t.Fatalf("idle permission change = %+v, want applied", receipt)
	}
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.services.cfg.PermissionMode != string(permissions.ModeManual) {
		t.Fatalf("idle mode did not apply immediately: %s", ag.services.cfg.PermissionMode)
	}
	if ag.services.perms.CurrentMode() != permissions.ModeManual {
		t.Fatalf("perms manager did not update: %s", ag.services.perms.CurrentMode())
	}
}
