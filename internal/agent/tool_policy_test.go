package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/hooks"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

func TestShadowReadRequiresApprovalAndUsesUnknownCapability(t *testing.T) {
	a := capabilityTestAgent(t)
	a.services.perms.SetMode(permissions.ModeDefault)
	shadow := &shadowCapabilityTool{}
	a.services.registry.RegisterIn("shadow", shadow)
	if kind := toolKind(shadow); kind != permissions.KindUnknown || kind.PlanAllowed() || kind.Concurrent() || !kind.NeedsReview() {
		t.Fatalf("shadow implementation inherited builtin policy: %v", kind)
	}
	events := watchTestEvents(t, a)
	requestedApproval := false
	done := make(chan bool, 1)
	go func() {
		_, failed := a.executeTool(messages.ToolCall{ID: "shadow-default", Name: "Read"})
		done <- failed
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event.Kind != protocol.EventApprovalRequest || event.Approval == nil {
				continue
			}
			requestedApproval = true
			_, err := a.Submit(context.Background(), protocol.Command{ID: "deny-shadow", SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: event.Approval.ID, Approve: false}})
			if err != nil {
				t.Fatal(err)
			}
		case failed := <-done:
			if !requestedApproval || !failed || shadow.runs.Load() != 0 {
				t.Fatal("same-named tool executed without user approval")
			}
			return
		case <-deadline.C:
			t.Fatal("shadow approval did not finish")
		}
	}
}

func TestToolHooksMergeBeforeSingleApproval(t *testing.T) {
	for _, tc := range []struct {
		name          string
		customAllow   bool
		goDecision    plugin.ToolDecision
		shellDecision hooks.Decision
		wantApproval  bool
		wantDenied    bool
	}{
		{"go-allow-shell-deny", false, plugin.DecisionAllow, hooks.DecisionDeny, false, true},
		{"custom-allow-go-deny", true, plugin.DecisionDeny, hooks.DecisionNone, false, true},
		{"custom-allow-shell-deny", true, plugin.DecisionNone, hooks.DecisionDeny, false, true},
		{"go-ask-shell-deny", false, plugin.DecisionAsk, hooks.DecisionDeny, false, true},
		{"go-ask-shell-allow", false, plugin.DecisionAsk, hooks.DecisionAllow, true, false},
		{"go-allow-shell-ask", false, plugin.DecisionAllow, hooks.DecisionAsk, true, false},
		{"two-asks", false, plugin.DecisionAsk, hooks.DecisionAsk, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := capabilityTestAgent(t)
			a.services.perms.SetMode(permissions.ModeDefault)
			shadow := &shadowCapabilityTool{}
			a.services.registry.RegisterIn("shadow", shadow)
			if tc.customAllow {
				a.discovery.customPerms["Read"] = "allow"
			}
			a.services.gohooks.AddPreTool(func(string, map[string]any) (plugin.ToolDecision, string) { return tc.goDecision, "Go policy" })
			if tc.shellDecision != "" {
				a.services.hooks.Update(hooks.Config{hooks.EventPreToolUse: []hooks.HookSpec{{Command: fmt.Sprintf(`printf '%%s' '{"decision":"%s","reason":"shell policy"}'`, tc.shellDecision)}}})
			}
			events := watchTestEvents(t, a)
			done := make(chan bool, 1)
			go func() { _, failed := a.executeTool(messages.ToolCall{ID: "merged-call", Name: "Read"}); done <- failed }()
			approvals := 0
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for {
				select {
				case event := <-events:
					if event.Kind != protocol.EventApprovalRequest || event.Approval == nil {
						continue
					}
					approvals++
					if !tc.wantApproval || approvals > 1 {
						t.Fatalf("unexpected approval #%d", approvals)
					}
					r, err := a.Submit(context.Background(), protocol.Command{ID: protocol.CommandID(fmt.Sprintf("approve-merged-%d", approvals)), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: event.Approval.ID, Approve: true}})
					if err != nil || r.Rejected() {
						t.Fatalf("approval: %+v %v", r, err)
					}
				case failed := <-done:
					if failed != tc.wantDenied || (approvals == 1) != tc.wantApproval {
						t.Fatalf("failed=%v approvals=%d", failed, approvals)
					}
					if got := shadow.runs.Load(); tc.wantDenied && got != 0 || !tc.wantDenied && got != 1 {
						t.Fatalf("tool ran %d times", got)
					}
					return
				case <-deadline.C:
					t.Fatal("merged hook decision did not settle")
				}
			}
		})
	}
}

func TestExternalNewDirectoryCapabilityUsesExistingParent(t *testing.T) {
	a := capabilityTestAgent(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "new", "nested", "result.txt")
	call := messages.ToolCall{Name: "Write", Arguments: map[string]any{"file_path": path, "content": "result"}}
	_, requests, _, err := a.toolCapabilityPolicy(call, tools.NewWriteTool())
	canonical, resolveErr := filepath.EvalSymlinks(parent)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if err != nil || len(requests) != 1 || requests[0].Path != canonical {
		t.Fatalf("inferred grant: %+v %v", requests, err)
	}
	call.Arguments["requested_capabilities"] = []any{map[string]any{"kind": "directory", "path": parent, "access": "write"}}
	_, requests, _, err = a.toolCapabilityPolicy(call, tools.NewWriteTool())
	if err != nil || len(requests) != 1 || requests[0].Path != canonical {
		t.Fatalf("explicit covering grant: %+v %v", requests, err)
	}
	// A same-named implementation does not infer filesystem access from what
	// could be unrelated business arguments.
	delete(call.Arguments, "requested_capabilities")
	_, requests, _, err = a.toolCapabilityPolicy(call, &shadowCapabilityTool{})
	if err != nil || len(requests) != 0 {
		t.Fatalf("shadow inferred file rights: %+v %v", requests, err)
	}
	if _, _, _, err = a.toolCapabilityPolicy(messages.ToolCall{Name: "Write", Arguments: map[string]any{"file_path": "/missing-root-file"}}, tools.NewWriteTool()); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("root grant was not rejected: %v", err)
	}
}
