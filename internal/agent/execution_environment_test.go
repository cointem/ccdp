package agent

import (
	"ccdp/internal/tooling"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/execution"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

func TestReviewerMalformedOrExhaustedDoesNotGrantAccess(t *testing.T) {
	for _, output := range []string{"", `{}`, `{"approved":"true"}`, `{"approved":true} trailing`, `{"approved":true,"extra":"ignored"}`, "```json\n{\"approved\":true}\n```"} {
		if _, valid := parseGuardianVerdict(output); valid {
			t.Fatalf("malformed review granted authority: %q", output)
		}
	}
	if verdict, valid := parseGuardianVerdict(`{"approved":false,"reason":"no"}`); !valid || verdict.Approved {
		t.Fatal("explicit reviewer denial lost")
	}
	a := capabilityTestAgent(t)
	a.mu.Lock()
	a.turnMemory.guardianUses = 3
	a.mu.Unlock()
	if err := a.guardianCheck(messages.ToolCall{Name: "Write"}); !errors.Is(err, errGuardianUnavailable) {
		t.Fatalf("exhausted reviewer: %v", err)
	}
}

func answerEnvironmentApproval(t *testing.T, a *Agent, events <-chan protocol.EventView, allow bool, scope protocol.CapabilityScope) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind != protocol.EventApprovalRequest || event.Approval == nil {
				continue
			}
			receipt := submitTestCommand(t, a, protocol.Command{ID: protocol.CommandID(nextRuntimeID("decision")), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: event.Approval.ID, Approve: allow, CapabilityScope: scope}})
			if receipt.Rejected() {
				t.Fatalf("approval rejected: %+v", receipt)
			}
			return
		case <-deadline:
			t.Fatal("access request not published")
		}
	}
}

func TestResourceRequestsShareDecisionAcrossRootAndChild(t *testing.T) {
	a := capabilityTestAgent(t)
	events := watchTestEvents(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, opts, err := childOptionsFromParent(a, childPurposeTask)
	if err != nil || opts.access == nil {
		t.Fatalf("child access channel: %v", err)
	}
	request, err := canonicalizeCapability(a.currentSandbox(), protocol.CapabilityRequest{Kind: "directory", Path: t.TempDir(), Access: "write"})
	if err != nil {
		t.Fatal(err)
	}
	answers := make(chan approvalAnswer, 2)
	go func() {
		answers <- a.requestApprovalDetailed(messages.ToolCall{ID: "root", Name: "Write", Arguments: map[string]any{"file_path": filepath.Join(request.Path, "a")}}, "new output directory", []protocol.CapabilityRequest{request}, true, ctx)
	}()
	go func() {
		answers <- opts.access.decide(ctx, messages.ToolCall{ID: "child", Name: "Write", Arguments: map[string]any{"file_path": filepath.Join(request.Path, "b")}}, "same output directory", []protocol.CapabilityRequest{request})
	}()
	answerEnvironmentApproval(t, a, events, true, protocol.CapabilityScopeSession)
	for i := 0; i < 2; i++ {
		select {
		case answer := <-answers:
			if !answer.approve || answer.capabilityScope != protocol.CapabilityScopeSession {
				t.Fatalf("answer: %+v", answer)
			}
		case <-ctx.Done():
			t.Fatal("duplicate approval did not coalesce")
		}
	}
	_, missing, _, err := a.toolCapabilityPolicy(messages.ToolCall{Name: "Write", Arguments: map[string]any{"file_path": filepath.Join(request.Path, "c")}}, tools.NewWriteTool())
	if err != nil || len(missing) != 0 {
		t.Fatalf("child session grant not available at root: %v %v", missing, err)
	}
	for {
		select {
		case event := <-events:
			if event.Kind == protocol.EventApprovalRequest {
				t.Fatal("duplicate modal for session grant")
			}
		default:
			return
		}
	}
}

func TestOnceDecisionIsNotSharedAndDeniedResourceIsCached(t *testing.T) {
	a := capabilityTestAgent(t)
	events := watchTestEvents(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	requests := []protocol.CapabilityRequest{{Kind: "directory", Path: t.TempDir(), Access: "write"}}
	answers := make(chan approvalAnswer, 2)
	for _, id := range []string{"once-a", "once-b"} {
		go func(id string) {
			answers <- a.requestApprovalDetailed(messages.ToolCall{ID: id, Name: "Write"}, "access", requests, true, ctx)
		}(id)
	}
	answerEnvironmentApproval(t, a, events, true, protocol.CapabilityScopeOnce)
	answerEnvironmentApproval(t, a, events, false, "")
	for i := 0; i < 2; i++ {
		select {
		case <-answers:
		case <-ctx.Done():
			t.Fatal("decision did not settle")
		}
	}
	a.mu.Lock()
	a.capabilities.capabilityRevision++
	a.mu.Unlock() // unrelated additive grant must not erase a denial
	answer := a.requestApprovalDetailed(messages.ToolCall{ID: "different-args", Name: "Bash", Arguments: map[string]any{"command": "different command"}}, "access", requests, true, ctx)
	if answer.approve || ctx.Err() != nil {
		t.Fatal("denial was lost or prompted again")
	}
}

func TestAutoRoutineWriteSkipsReviewerAndDeniedResourceSkipsItAgain(t *testing.T) {
	a := capabilityTestAgent(t)
	a.mu.Lock()
	a.services.cfg.EnableGuardian = config.BoolPtr(true)
	a.mu.Unlock()
	path := filepath.Join(a.configSnapshot().Workspace, "automatic.txt")
	if out, failed := executeTestTool(t, a, messages.ToolCall{ID: "ordinary", Name: "Write", Arguments: map[string]any{"file_path": path, "content": "ok"}}); failed {
		t.Fatal(out)
	}
	call := messages.ToolCall{ID: "cached", Name: "Write"}
	requests := []protocol.CapabilityRequest{{Kind: "directory", Path: t.TempDir(), Access: "write"}}
	key := a.resourceDecisionKey(call, requests, true)
	a.mu.Lock()
	a.cacheResourceDecisionLocked(key, approvalAnswer{})
	a.mu.Unlock()
	authority := toolAuthority{agent: a, call: call, capabilities: requests, execution: &toolJournalExecution{}}
	if authority.Approve("access", true) {
		t.Fatal("cached denial allowed")
	}
	a.mu.Lock()
	uses := a.turnMemory.guardianUses
	a.mu.Unlock()
	if uses != 0 {
		t.Fatalf("routine/cached call invoked reviewer %d times", uses)
	}
	result := tooling.Result{IsError: true, Failure: authority.execution.failure}
	for i := 1; i <= 3; i++ {
		if stopped := a.repeatedAccessDenial(1, []tooling.Result{result}); stopped != (i == 3) {
			t.Fatalf("denial round %d stopped=%v", i, stopped)
		}
	}
	if a.repeatedAccessDenial(2, []tooling.Result{result}) {
		t.Fatal("new user turn inherited loop counter")
	}
}

func TestQuarantinedWorkspaceAllowsChatAndIndependentWorkspace(t *testing.T) {
	a := capabilityTestAgent(t)
	old := a.currentSandbox().Snapshot()
	old.MarkExternalExecution()
	if err := old.RecordQuarantine("test unconfirmed execution"); err != nil {
		t.Fatal(err)
	}
	if err := execution.Quarantine(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.capabilities.capabilityRevoking = true
	a.mu.Unlock()
	if err := a.toolExecutionError(); err != nil {
		t.Fatalf("chat runtime poisoned: %v", err)
	}
	if _, _, _, err := a.toolCapabilityPolicy(messages.ToolCall{Name: "ReadOutput"}, &tools.ReadOutputTool{}); err != nil {
		t.Fatalf("saved outputs blocked: %v", err)
	}
	if _, _, _, err := a.toolCapabilityPolicy(messages.ToolCall{Name: "Write"}, tools.NewWriteTool()); err == nil {
		t.Fatal("quarantined write admitted")
	}
	previousScratch := a.services.resources.ScratchDir()
	independent := t.TempDir()
	if err := a.SetWorkspace(independent); err != nil {
		t.Fatalf("independent workspace: %v", err)
	}
	if a.services.resources.ScratchDir() == previousScratch {
		t.Fatal("unconfirmed cache reused by independent workspace")
	}
	if err := old.CheckExecution(); err == nil {
		t.Fatal("workspace switch cleared old quarantine")
	}
	if err := execution.CheckEnvironment(a.currentSandbox()); err != nil {
		t.Fatal(err)
	}
	if out, failed := executeTestTool(t, a, messages.ToolCall{ID: "new-workspace", Name: "Write", Arguments: map[string]any{"file_path": filepath.Join(independent, "ready.txt"), "content": "ready"}}); failed {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(independent, "ready.txt")); err != nil {
		t.Fatal(err)
	}
}
