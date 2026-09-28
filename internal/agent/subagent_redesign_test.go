package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

func TestSharedWorkerDefaultCanWriteWithoutGit(t *testing.T) {
	f := &fakeLLM{script: []string{"tool:Write|file_path=child.txt;mode=create;content=created", "tool:Bash|command=printf verified", "text:done"}}
	a, _ := newTestAgent(t, f)
	rows, err := runTestChildren(a, []childTask{{Description: "write and verify"}}, "worker")
	if err != nil || len(rows) != 1 || rows[0].Error != "" {
		t.Fatalf("%+v %v", rows, err)
	}
	data, err := os.ReadFile(filepath.Join(a.cfg.Workspace, "child.txt"))
	if err != nil || string(data) != "created" {
		t.Fatalf("write=%q %v", data, err)
	}
	if rows[0].Role != "worker" || rows[0].WorkspaceMode != "shared" || rows[0].Workspace != a.cfg.Workspace {
		t.Fatalf("wrong defaults: %+v", rows)
	}
	saved, err := LoadSession(a.cfg.SessionDir, rows[0].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range saved.History {
		if m.Role == messages.RoleTool && strings.Contains(m.Content, "verified") {
			found = true
		}
	}
	if !found {
		t.Fatal("child could not execute Bash")
	}
}

func TestExplorerCanReportButCannotWrite(t *testing.T) {
	f := &fakeLLM{script: []string{"tool:Write|file_path=forbidden.txt;mode=create;content=bad", "tool:SendMessage|agent_id=parent;text=write is blocked", "tool:SendMessage|agent_id=parent;text=inspection complete", "text:reported"}}
	a, _ := newTestAgent(t, f)
	rows, err := runTestChildren(a, []childTask{{Description: "inspect", Role: "explorer"}}, "explorer")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("%+v %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.Workspace, "forbidden.txt")); !os.IsNotExist(err) {
		t.Fatal("explorer wrote")
	}
	if !a.hasAgentMessage() {
		t.Fatal("explorer report missing")
	}
	a.mu.Lock()
	busy := a.busy
	messages := len(a.pendingInputs)
	a.mu.Unlock()
	if messages != 2 {
		t.Fatalf("expected two mid-run reports, got %d", messages)
	}
	if busy {
		t.Fatal("message started idle parent")
	}
}

func TestAgentMailboxDurableAndDoesNotStartTurn(t *testing.T) {
	a := newRuntimeAgent(t)
	cmd := protocol.NewSubmitInput("mail", "", "mail", "collaboration", protocol.InputMessage)
	if receipt := a.applySubmitInput(cmd); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	a.mu.Lock()
	busy, starting := a.busy, a.hasTurnStartingInputLocked()
	a.mu.Unlock()
	if busy || starting || !a.hasAgentMessage() {
		t.Fatal("queue-only message changed turn admission")
	}
	saved, err := LoadSession(a.cfg.SessionDir, a.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.PendingInputs) != 1 || saved.PendingInputs[0].Strategy != protocol.InputMessage {
		t.Fatalf("mail replay: %+v", saved.PendingInputs)
	}
	if err := a.consumeAgentMessages(); err != nil {
		t.Fatal(err)
	}
	if a.hasAgentMessage() {
		t.Fatal("message not consumed")
	}
}

func TestAgentMessageDoesNotBlockUserSteering(t *testing.T) {
	a := newRuntimeAgent(t)
	if receipt := a.applySubmitInput(protocol.NewSubmitInput("mail-first", "", "mail-first", "progress", protocol.InputMessage)); receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	a.mu.Lock()
	a.busy = true
	a.mu.Unlock()
	receipt := a.applySubmitInput(protocol.NewSubmitInput("steer-next", "", "steer-next", "new direction", protocol.InputSteer))
	a.mu.Lock()
	a.busy = false
	a.mu.Unlock()
	if receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	input, ok, err := a.claimPendingInputAndAppend("turn-1", protocol.InputSteer)
	if err != nil || !ok || input.Text != "new direction" || !a.hasAgentMessage() {
		t.Fatalf("message blocked steering: %+v %v %v", input, ok, err)
	}
}

func TestChildDormantMessagesSurviveContinuation(t *testing.T) {
	f := &fakeLLM{script: []string{"text:first", "text:second"}}
	a, _ := newTestAgent(t, f)
	rows, err := runTestChildren(a, []childTask{{Description: "first"}}, "first")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("%+v %v", rows, err)
	}
	id := protocol.SessionID(rows[0].SessionID)
	if err := a.sendAgentMessage(context.Background(), "queued-message", string(id), "remember this"); err != nil {
		t.Fatal(err)
	}
	r, _ := a.supervisor.lookup(id)
	r.mu.Lock()
	pending := len(r.fact.PendingMessages)
	r.mu.Unlock()
	if pending != 1 {
		t.Fatal("dormant message not retained")
	}
	run, err := a.Sessions().Control(context.Background(), protocol.AgentControl{ID: "continue-child", SessionID: id, RunID: protocol.RunID(rows[0].RunID), Action: "continue", Text: "second"})
	if err != nil {
		t.Fatal(err)
	}
	next, _ := a.supervisor.lookup(id)
	select {
	case <-next.done:
	case <-time.After(8 * time.Second):
		t.Fatal("continuation hung")
	}
	if run.ID == protocol.RunID(rows[0].RunID) {
		t.Fatal("run identity reused")
	}
	saved, err := LoadSession(a.cfg.SessionDir, string(id))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, m := range saved.History {
		if strings.Contains(m.Content, "remember this") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("message delivered %d times", found)
	}
}

func TestIsolatedNonGitIncludesCurrentFiles(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{script: []string{"text:done"}})
	if err := os.WriteFile(filepath.Join(a.cfg.Workspace, "untracked.txt"), []byte("working content"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := runTestChildren(a, []childTask{{Description: "inspect", WorkspaceMode: "isolated"}}, "isolated")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("%+v %v", rows, err)
	}
	dir := rows[0].Workspace
	t.Cleanup(func() {
		if err := a.removeChildWorkspace(context.Background(), dir); err != nil {
			t.Error(err)
		}
	})
	data, err := os.ReadFile(filepath.Join(dir, "untracked.txt"))
	if err != nil || string(data) != "working content" {
		t.Fatalf("%q %v", data, err)
	}
	if dir == a.cfg.Workspace {
		t.Fatal("not isolated")
	}
}

func TestSharedWorkerRetainsParentDeny(t *testing.T) {
	p := &childRuntimeTestProvider{name: "deny-test", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	a.perms.RememberDeny(permissions.SessionKey("Write", map[string]any{"file_path": "x"}))
	cfg, opts, err := childOptionsFromParent(a, childPurposeTask)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := resolveChildTask(childTask{})
	_, _, err = a.prepareChildWorkspace(context.Background(), task, &cfg, &opts)
	if err != nil {
		t.Fatal(err)
	}
	if decision, _ := opts.Permissions.Check("Write", map[string]any{"file_path": "x"}); decision != permissions.DecisionDeny {
		t.Fatalf("lost deny: %v", decision)
	}
	if !opts.AllowedTools["Bash"] {
		t.Fatal("worker tools were narrowed")
	}
}

func TestSharedWorkerCannotEscapeParentPlan(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{script: []string{"tool:Write|file_path=escape.txt;mode=create;content=bad", "text:blocked"}})
	if err := a.setExecutionMode(true); err != nil {
		t.Fatal(err)
	}
	rows, err := runTestChildren(a, []childTask{{Description: "try writing"}}, "plan-child")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("%+v %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.Workspace, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("worker escaped parent plan restriction")
	}
}

func TestExplorerRetainsParentDecisionHooks(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{script: []string{"text:done"}})
	a.cfg.Hooks = hooks.Config{hooks.EventPreToolUse: {{Matcher: "Read", Command: "exit 2"}}}
	cfg, opts, err := childOptionsFromParent(a, childPurposeTask)
	if err != nil {
		t.Fatal(err)
	}
	applyChildRole(&cfg, &opts, "explorer", "shared")
	if len(cfg.Hooks[hooks.EventPreToolUse]) != 1 || !opts.ReadOnlyWorkspace || opts.AllowedTools["Bash"] {
		t.Fatal("explorer lost parent decision hooks or read-only tool cap")
	}
}

func TestIsolatedChildCanContinue(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{script: []string{"text:first", "text:second"}})
	rows, err := runTestChildren(a, []childTask{{Description: "first", WorkspaceMode: "isolated"}}, "isolated-continue")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("%+v %v", rows, err)
	}
	t.Cleanup(func() { _ = a.removeChildWorkspace(context.Background(), rows[0].Workspace) })
	id := protocol.SessionID(rows[0].SessionID)
	_, err = a.Sessions().Control(context.Background(), protocol.AgentControl{ID: "isolated-next", SessionID: id, RunID: protocol.RunID(rows[0].RunID), Action: "continue", Text: "second"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := a.supervisor.lookup(id)
	select {
	case <-r.done:
	case <-time.After(8 * time.Second):
		t.Fatal("isolated continuation hung")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
}

func TestChildForkCopiesBalancedPrefix(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{script: []string{"text:child finished"}})
	a.mu.Lock()
	a.history = []messages.Message{
		{Role: messages.RoleUser, Content: "parent context"},
		{Role: messages.RoleAssistant, Content: "prior answer"},
		{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "pending", Name: "SpawnAgent"}}},
	}
	a.mu.Unlock()
	rows, err := runTestChildren(a, []childTask{{Description: "child assignment", ContextMode: "fork"}}, "fork")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("%+v %v", rows, err)
	}
	saved, err := LoadSession(a.cfg.SessionDir, rows[0].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ParentID != a.sessionID || len(saved.History) < 4 || saved.History[0].Content != "parent context" {
		t.Fatalf("fork lost history/lineage: %+v", saved)
	}
	for _, m := range saved.History {
		for _, call := range m.ToolCalls {
			if call.ID == "pending" {
				t.Fatal("fork copied pending tool call")
			}
		}
	}
}

func TestWorkspaceMutationBlocksSharedWorkerAdmission(t *testing.T) {
	a, _ := newTestAgent(t, &fakeLLM{script: []string{"text:done"}})
	release, err := a.reserveWorkspaceMutation()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := runTestChildren(a, []childTask{{Description: "blocked"}}, "blocked")
	if err != nil || rows[0].Error == "" {
		t.Fatalf("shared worker admitted during mutation: %+v %v", rows, err)
	}
	release()
	rows, err = runTestChildren(a, []childTask{{Description: "allowed"}}, "allowed")
	if err != nil || rows[0].Error != "" {
		t.Fatalf("reservation not released: %+v %v", rows, err)
	}
}

func TestSharedWorkerBlocksWorkspaceMutationUntilSettled(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	p := &childRuntimeTestProvider{name: "busy-worker", stream: func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		close(started)
		select {
		case <-release:
			return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
		case <-ctx.Done():
			return llm.StreamResult{}, ctx.Err()
		}
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := a.supervisor.launch(a, ctx, childTask{Description: "wait"}, childPurposeTask, "busy")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	if unlock, err := a.reserveWorkspaceMutation(); err == nil {
		unlock()
		t.Fatal("workspace mutation admitted while shared worker active")
	}
	close(release)
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not settle")
	}
	unlock, err := a.reserveWorkspaceMutation()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
