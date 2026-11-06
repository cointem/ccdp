package agent

import (
	"testing"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

func TestApprovedToolRechecksCurrentHardDeny(t *testing.T) {
	a := capabilityTestAgent(t)
	a.services.perms.SetMode(permissions.ModeManual)
	shadow := &shadowCapabilityTool{}
	a.services.registry.RegisterIn("shadow", shadow)
	events := watchTestEvents(t, a)
	done := make(chan bool, 1)
	go func() {
		_, failed := executeTestTool(t, a, messages.ToolCall{ID: "pending-deny", Name: "Read", Arguments: map[string]any{}})
		done <- failed
	}()
	seen := drainUntil(t, events, 5*time.Second, func(ev protocol.EventView) bool { return ev.Kind == protocol.EventApprovalRequest })
	approval := seen[len(seen)-1].Approval
	cmd := protocol.Command{ID: "hard-deny-now", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandSetPermissionPolicy, PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{AlwaysDeny: []string{"Read:*"}}}}
	if receipt := a.applyCommand(cmd); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	if denied, _ := a.services.perms.HardDeny("Read", map[string]any{}); !denied {
		t.Fatal("hard deny fixture failed")
	}
	receipt := submitTestCommand(t, a, protocol.Command{ID: "approve-stale-deny", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: approval.ID, Approve: true}})
	if receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tool stuck")
	}
	if shadow.runs.Load() > 0 {
		t.Fatalf("tool ran %d times under explicit current hard deny", shadow.runs.Load())
	}
}
