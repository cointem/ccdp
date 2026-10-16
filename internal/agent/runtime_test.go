package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/fsops"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// truncateForLog shortens a message for test failure dumps.
func truncateForLog(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// newRuntimeAgent builds an agent for runtime-loop tests with its event
// channel drained in the background.
func newRuntimeAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	ag, err := testNew(t, &cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Cleanups run LIFO: Close() first, then close the event channel so no
	// late emit can panic on a closed channel.
	t.Cleanup(ag.Close)
	watchTestEvents(t, ag)
	return ag
}

func TestEstimateTokensUsesRealBaseline(t *testing.T) {
	ag := newRuntimeAgent(t)

	// No provider usage yet: pure estimation.
	full := ag.estimateTokens()

	// Simulate a provider-reported prompt of 10000 tokens anchored at the
	// current history length; a later huge message must push the estimate
	// beyond the anchor.
	ag.mu.Lock()
	ag.conversation.durable.History = append(ag.conversation.durable.History, messages.Message{Role: messages.RoleUser, Content: "q"})
	anchored := len(ag.conversation.durable.History)
	ag.turnMemory.tokenBaseline.promptTokens = 10000
	ag.turnMemory.tokenBaseline.historyLen = anchored
	ag.mu.Unlock()

	if got := ag.estimateTokens(); got < 10000 {
		t.Fatalf("baseline anchor lost: got %d", got)
	}
	ag.mu.Lock()
	ag.conversation.durable.History = append(ag.conversation.durable.History, messages.Message{Role: messages.RoleUser, Content: strings.Repeat("x", 8000)})
	ag.mu.Unlock()
	if got, want := ag.estimateTokens(), 10000+llm.EstimateTokens(strings.Repeat("x", 8000))+4; got < want-50 || got > want+50 {
		t.Errorf("estimate = %d, want ≈ %d (anchor + incremental)", got, want)
	}

	// A history rewrite (compaction) invalidates the anchor.
	ag.mu.Lock()
	ag.turnMemory.tokenBaseline.promptTokens = 0
	ag.turnMemory.tokenBaseline.historyLen = 0
	ag.mu.Unlock()
	if got := ag.estimateTokens(); got >= 10000 {
		t.Errorf("invalidated baseline should fall back to estimation, got %d", got)
	}
	_ = full
}

func TestInterruptNoteInjectedOnce(t *testing.T) {
	ag := newRuntimeAgent(t)

	ag.mu.Lock()
	ag.turnMemory.interruptNote = true
	ag.mu.Unlock()

	firstStep := ag.beginStep()
	req := ag.buildRequestForStep(firstStep)
	releaseStepLease(firstStep)
	sys, _ := req.Messages[0].Content.(string)
	if !strings.Contains(sys, "Interrupted turn") {
		t.Error("interrupted-turn guidance missing from system prompt")
	}

	// Second request must not repeat it.
	secondStep := ag.beginStep()
	req = ag.buildRequestForStep(secondStep)
	releaseStepLease(secondStep)
	sys, _ = req.Messages[0].Content.(string)
	if strings.Contains(sys, "Interrupted turn") {
		t.Error("interrupted-turn guidance should be injected only once")
	}
}

func TestDispatchToolsInterruptedFillsEveryResult(t *testing.T) {
	ag := newRuntimeAgent(t)

	calls := []messages.ToolCall{
		{ID: "c1", Name: "Read", Arguments: map[string]any{}},
		{ID: "c2", Name: "Read", Arguments: map[string]any{}},
		{ID: "c3", Name: "Read", Arguments: map[string]any{}},
	}
	// Simulate an interrupt before dispatch.
	ag.mu.Lock()
	ag.turn.interruptFlag = true
	ag.mu.Unlock()

	results := ag.dispatchTools(calls)
	if len(results) != len(calls) {
		t.Fatalf("expected %d results, got %d", len(calls), len(results))
	}
	// Every result must be filled: an empty output would leave the assistant
	// message with tool_calls that have no matching tool result, which
	// providers reject with 400 on the next request.
	for i, r := range results {
		if r.output == "" {
			t.Errorf("call %d (%s): empty result would break the protocol", i, calls[i].ID)
		}
		if !r.isErr {
			t.Errorf("call %d: interrupted dispatch should be an error result", i)
		}
	}
}

func TestRequestApprovalConcurrentSerialized(t *testing.T) {
	ag := newRuntimeAgent(t)

	// A live turn context so requestApproval's select works.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.mu.Lock()
	ag.turn.turnCtx = ctx
	ag.turn.turnCancel = cancel
	ag.mu.Unlock()

	answer := func(ev protocol.EventView) {
		submitTestCommand(t, ag, protocol.Command{ID: protocol.CommandID(nextRuntimeID("approve")), SessionID: protocol.SessionID(ag.SessionID()),
			Type:     protocol.CommandApproveTool,
			Approval: &protocol.ApproveTool{ApprovalID: ev.Approval.ID, Approve: true}})
	}

	type verdict struct {
		approved bool
	}
	worker := func(tc messages.ToolCall, ch chan verdict) {
		ok, _ := ag.requestApproval(tc, "test")
		ch <- verdict{approved: ok}
	}

	// Two DIFFERENT invocations: each gets its own modal, one after another
	// (the owner decision slot must never overlap modals).
	ch1 := make(chan verdict, 1)
	ch2 := make(chan verdict, 1)
	go worker(messages.ToolCall{ID: "t1", Name: "Bash", Arguments: map[string]any{"command": "make build"}}, ch1)
	go worker(messages.ToolCall{ID: "t2", Name: "Bash", Arguments: map[string]any{"command": "make test"}}, ch2)

	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case ev, ok := <-watchTestEvents(t, ag):
			if !ok {
				t.Fatal("event channel closed before both approvals surfaced")
			}
			if ev.Kind == protocol.EventApprovalRequest && ev.Approval != nil {
				seen[ev.Approval.ID] = true
				answer(ev)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for both approval modals; seen=%v", seen)
		}
	}
	if !seen["t1"] || !seen["t2"] {
		t.Errorf("both approval requests must surface, got %v", seen)
	}
	select {
	case v := <-ch1:
		if !v.approved {
			t.Error("worker 1 denied, expected approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker 1 never finished")
	}
	select {
	case v := <-ch2:
		if !v.approved {
			t.Error("worker 2 denied, expected approval")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker 2 never finished")
	}
}

func TestRequestApprovalCacheReplaysDecision(t *testing.T) {
	ag := newRuntimeAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.mu.Lock()
	ag.turn.turnCtx = ctx
	ag.turn.turnCancel = cancel
	ag.mu.Unlock()

	// Two identical parallel invocations: only ONE modal may appear; the
	// second worker replays the cached decision (Codex's approval cache).
	type verdict struct {
		approved bool
	}
	tc := messages.ToolCall{ID: "t1", Name: "Bash", Arguments: map[string]any{"command": "make build"}}
	ch1 := make(chan verdict, 1)
	ch2 := make(chan verdict, 1)
	go func() { ok, _ := ag.requestApproval(tc, "test"); ch1 <- verdict{ok} }()
	go func() { ok, _ := ag.requestApproval(tc, "test"); ch2 <- verdict{ok} }()

	modals := 0
	deadline := time.After(5 * time.Second)
	for modals < 1 {
		select {
		case ev, ok := <-watchTestEvents(t, ag):
			if !ok {
				t.Fatal("event channel closed before the approval surfaced")
			}
			if ev.Kind == protocol.EventApprovalRequest && ev.Approval != nil {
				modals++
				submitTestCommand(t, ag, protocol.Command{ID: protocol.CommandID(nextRuntimeID("approve")), SessionID: protocol.SessionID(ag.SessionID()),
					Type:     protocol.CommandApproveTool,
					Approval: &protocol.ApproveTool{ApprovalID: ev.Approval.ID, Approve: true}})
			}
		case <-deadline:
			t.Fatal("timed out waiting for the approval modal")
		}
	}
	// Drain remaining events briefly; no second modal may arrive.
drained:
	for {
		select {
		case ev, ok := <-watchTestEvents(t, ag):
			if !ok {
				break drained
			}
			if ev.Kind == protocol.EventApprovalRequest && ev.Approval != nil {
				t.Fatalf("second approval modal surfaced for an identical call: %s", ev.Approval.ID)
			}
		case <-time.After(300 * time.Millisecond):
			break drained
		}
	}
	for _, ch := range []chan verdict{ch1, ch2} {
		select {
		case v := <-ch:
			if !v.approved {
				t.Error("worker denied, expected the cached approval")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("worker never finished")
		}
	}
}

// TestTruncatedToolCallsVoided pins pi's failToolCallsFromTruncatedMessage
// behavior: tool calls carried by a length-truncated reply are never executed;
// they get error results so the model can re-issue them.
func TestTruncatedToolCallsVoided(t *testing.T) {
	f := &fakeLLM{script: []string{
		"truncated:Bash|command=echo should-not-run",
		"text:done",
	}}
	ag, events := newTestAgent(t, f)
	go ag.Run()

	submitTestInput(t, ag, "run it")

	saw := drainUntil(t, events, 10*time.Second, func(ev protocol.EventView) bool {
		return ev.Kind == protocol.EventToolResult && ev.Tool != nil && ev.Tool.Status == "denied"
	})
	found := false
	for _, ev := range saw {
		if ev.Tool != nil && ev.Tool.Name == "Bash" && strings.Contains(ev.Tool.Output, "truncated") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a voided (truncated) tool event, got %+v", saw)
	}

	// The tool result in history must be an error telling the model to
	// re-issue, and the command must not have executed.
	ag.mu.Lock()
	var toolMsg string
	for i := len(ag.conversation.durable.History) - 1; i >= 0; i-- {
		if ag.conversation.durable.History[i].Role == messages.RoleTool {
			toolMsg = ag.conversation.durable.History[i].Content
			break
		}
	}
	ag.mu.Unlock()
	if !strings.Contains(toolMsg, "not executed") || !strings.Contains(toolMsg, "Re-issue") {
		t.Fatalf("expected re-issue error tool result, got %q", toolMsg)
	}
	if strings.Contains(toolMsg, "should-not-run") {
		t.Fatal("truncated tool call was executed; it must be voided")
	}
}

// TestSteerQueuedMessageInjected pins the steering semantics: a message sent
// while the agent is busy is injected into the RUNNING turn at the next
// tool-result boundary, not deferred to a follow-up turn.
func TestSteerQueuedMessageInjected(t *testing.T) {
	f := &fakeLLM{script: []string{
		"tool:Bash|command=sleep 0.4",
		"text:done",
	}}
	ag, events := newTestAgent(t, f)
	go ag.Run()

	submitTestInput(t, ag, "first message")
	time.Sleep(100 * time.Millisecond) // the turn is now inside the Bash call
	submitTestInput(t, ag, "steered message")

	drainUntil(t, events, 10*time.Second, func(ev protocol.EventView) bool {
		return ev.Kind == protocol.EventTurnDone
	})

	ag.mu.Lock()
	defer ag.mu.Unlock()
	// Expected shape: [user1, catalog, asst(tool), tool, user2(steered), asst(done)]
	// The base system prompt lives outside history; catalog deltas are durable.
	var dump strings.Builder
	for i, m := range ag.conversation.durable.History {
		fmt.Fprintf(&dump, "%d:%s:%q ", i, m.Role, truncateForLog(m.Content))
	}
	t.Logf("history: %s", dump.String())
	n := len(ag.conversation.durable.History)
	if n != 6 {
		t.Fatalf("steered history should be 6 messages including catalog, got %d", n)
	}
	last := ag.conversation.durable.History[n-1]
	prev := ag.conversation.durable.History[n-2]
	if last.Role != messages.RoleAssistant || !strings.Contains(last.Content, "done") {
		t.Fatalf("expected final assistant 'done', got %q (%s)", last.Content, last.Role)
	}
	if prev.Role != messages.RoleUser || prev.Content != "steered message" {
		t.Fatalf("steered message must be the last user message before the reply, got %q (%s)", prev.Content, prev.Role)
	}
}

// TestForkBranchesSession pins the session-branch behavior: Fork creates and
// persists a child carrying history[:keep] plus a branch note, while leaving
// the live source Agent and its session-scoped resources untouched.
func TestForkBranchesSession(t *testing.T) {
	f := &fakeLLM{script: []string{
		"tool:Bash|command=echo one",
		"text:final answer for the first turn",
		"text:a branch summary of the abandoned direction",
	}}
	ag, events := newTestAgent(t, f)
	go ag.Run()

	submitTestInput(t, ag, "try direction A")
	drainUntil(t, events, 10*time.Second, func(ev protocol.EventView) bool {
		return ev.Kind == protocol.EventTurnDone
	})

	oldID := ag.SessionID()
	// turnFinished (which persists the session) runs after the TurnDone event
	// is emitted; poll briefly for the source snapshot to land on disk.
	var oldSnap *SessionSnapshot
	var err error
	for i := 0; i < 40; i++ {
		if oldSnap, err = LoadSession(ag.SessionDir(), oldID); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("source session not saved: %v", err)
	}
	oldCount := len(oldSnap.History)

	oldPersistence := ag.persistenceHandle()
	oldResources := ag.services.resources
	oldHistory := ag.History()
	newID, err := ag.Fork(1) // keep [user1], drop the tool round
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if newID == oldID {
		t.Fatal("fork reused the source session id")
	}
	if ag.SessionID() != oldID {
		t.Fatalf("fork changed source session id to %q", ag.SessionID())
	}
	if parent, point := ag.Lineage(); parent != "" || point != 0 {
		t.Fatalf("source lineage changed to (%q, %d)", parent, point)
	}
	if ag.persistenceHandle() != oldPersistence || ag.services.resources != oldResources {
		t.Fatal("fork replaced source persistence or resources")
	}
	if got := ag.History(); len(got) != len(oldHistory) {
		t.Fatalf("source history changed from %d to %d messages", len(oldHistory), len(got))
	}

	snap, err := LoadSession(ag.SessionDir(), newID)
	if err != nil {
		t.Fatalf("branched session not saved: %v", err)
	}
	if snap.ParentID != oldID || snap.BranchPoint != 1 {
		t.Fatalf("snapshot lineage = (%q, %d)", snap.ParentID, snap.BranchPoint)
	}
	if len(snap.History) != 2 { // branch-note + user1
		t.Fatalf("branched history should be note+user (2), got %d: %v", len(snap.History), snap.History)
	}
	if snap.History[0].Role != messages.RoleSystem || !strings.Contains(snap.History[0].Content, "branched from session "+oldID) {
		t.Fatalf("expected branch note first, got %q", snap.History[0].Content)
	}
	if !strings.Contains(snap.History[0].Content, "branch summary") {
		t.Fatalf("expected the abandoned-direction summary in the note, got %q", snap.History[0].Content)
	}
	// The child writer is closed before Fork returns and can be opened by the
	// later explicit OpenSession/Resume path.
	childWriter, err := session.OpenJSONLStore(ag.SessionDir(), newID)
	if err != nil {
		t.Fatalf("child writer remains locked: %v", err)
	}
	if err := childWriter.Close(); err != nil {
		t.Fatal(err)
	}
	// The source session file keeps its full history.
	after, err := LoadSession(ag.SessionDir(), oldID)
	if err != nil {
		t.Fatalf("source session missing after fork: %v", err)
	}
	if len(after.History) != oldCount {
		t.Fatalf("source session mutated by fork: %d → %d", oldCount, len(after.History))
	}
}

// TestPostCompactFileAttachments pins the post-compaction file restore: files
// read earlier are re-attached to the summary, files still referenced by the
// kept tail are skipped, and missing files are dropped.
func TestPostCompactFileAttachments(t *testing.T) {
	ag := newRuntimeAgent(t)
	dir := t.TempDir()
	old := strings.Repeat("old file content\n", 10)
	fresh := "fresh file content"
	if err := os.WriteFile(dir+"/old.go", []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/fresh.go", []byte(fresh), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir + "/old.go", dir + "/fresh.go"} {
		v, err := fsops.Observe(path)
		if err != nil {
			t.Fatal(err)
		}
		ag.services.resources.Files.MarkVersion(path, v)
	}

	// fresh.go is still referenced by the kept tail → skipped; old.go attached.
	tail := []messages.Message{{
		Role: messages.RoleAssistant,
		ToolCalls: []messages.ToolCall{{
			ID: "c1", Name: "Read", Arguments: map[string]any{"file_path": dir + "/fresh.go"},
		}},
	}}
	out := ag.postCompactFileAttachments(tail)
	if !strings.Contains(out, "old file content") {
		t.Fatalf("old.go content missing from attachments:\n%s", out)
	}
	if strings.Contains(out, "fresh file content") {
		t.Fatal("kept-tail file should not be re-attached")
	}

	// A file that disappeared is dropped silently.
	if err := os.Remove(dir + "/old.go"); err != nil {
		t.Fatal(err)
	}
	if out := ag.postCompactFileAttachments(nil); strings.Contains(out, "old file content") {
		t.Fatal("deleted file should not be attached")
	}
}

// TestPendingInboxPersisted pins the persistent inbox: queued user messages
// ride in the session snapshot and are restored on resume.
func TestPendingInboxPersisted(t *testing.T) {
	ag := newRuntimeAgent(t)

	// Admit messages through the typed command path while a turn is running.
	// InputQueued facts are the durable source of truth for resume.
	ag.mu.Lock()
	ag.turn.busy = true
	ag.mu.Unlock()
	// Seed usage through the durable projection, as production accounting does.
	if err := ag.persistenceHandle().persistUsage(Usage{InputTokens: 12, OutputTokens: 3, TurnCount: 2}); err != nil {
		t.Fatalf("persist usage fixture: %v", err)
	}
	for i, text := range []string{"queued one", "queued two", "queued three"} {
		cmd := protocol.NewSubmitInput(protocol.CommandID(fmt.Sprintf("pending-command-%d", i)), protocol.SessionID(ag.SessionID()), protocol.InputID(fmt.Sprintf("pending-input-%d", i)), text, protocol.InputFollowup)
		if receipt := ag.applyCommand(cmd); receipt.Rejected() {
			t.Fatalf("submit pending input %q: %+v", text, receipt)
		}
	}

	snap, err := LoadSession(ag.SessionDir(), ag.SessionID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(snap.PendingInputs) != 3 || snap.PendingInputs[0].Text != "queued one" {
		t.Fatalf("snapshot pending = %v", snap.PendingInputs)
	}

	// Release the original writer before opening the resumed handle.
	ag.Close()
	resumed, err := Resume(ag.services.cfg, snap)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer resumed.Close()
	resumed.mu.Lock()
	got := append([]protocol.InputView(nil), resumed.conversation.durable.PendingInputViews()...)
	resumed.mu.Unlock()
	if len(got) != 3 || got[2].Text != "queued three" {
		t.Fatalf("resumed pending inbox = %v", got)
	}
	if gotUsage := resumed.Usage(); gotUsage.TurnCount != 2 {
		t.Fatalf("resumed usage = %+v", gotUsage)
	}
}

func TestResumeRestoresWorkspaceAndModel(t *testing.T) {
	current := t.TempDir()
	restored := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = current
	cfg.NoSessionPersistence = true
	cfg.SessionDir = filepath.Join(current, "sessions")
	snap := &SessionSnapshot{
		ID:        "restore-context",
		CreatedAt: time.Now(),
		Workspace: restored,
		Model:     "restored-model",
		Usage:     Usage{TurnCount: 4},
	}
	ag, err := Resume(&cfg, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	if ag.services.cfg.Workspace != restored || ag.services.cfg.Model != "restored-model" {
		t.Fatalf("restored context = %q/%q", ag.services.cfg.Workspace, ag.services.cfg.Model)
	}
	if ag.Usage().TurnCount != 4 {
		t.Fatalf("restored usage = %+v", ag.Usage())
	}
}

func TestApprovalResponsesMustMatchPendingID(t *testing.T) {
	ag := newRuntimeAgent(t)
	approvalCh := make(chan approvalAnswer, 1)
	planCh := make(chan bool, 1)
	ag.mu.Lock()
	ag.decisions.pendingApproval = &ApprovalRequest{ID: "approval-new"}
	ag.decisions.approvalResp = approvalCh
	ag.plan.pendingPlan = &PlanRequest{ID: "plan-new"}
	ag.plan.planResp = planCh
	ag.mu.Unlock()

	submitTestCommand(t, ag, protocol.Command{ID: protocol.CommandID(nextRuntimeID("stale-approval")), SessionID: protocol.SessionID(ag.SessionID()),
		Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: "approval-old", Approve: true}})
	submitTestCommand(t, ag, protocol.Command{ID: protocol.CommandID(nextRuntimeID("stale-plan")), SessionID: protocol.SessionID(ag.SessionID()),
		Type: protocol.CommandApprovePlan, Plan: &protocol.ApprovePlan{PlanID: "plan-old", Approve: true}})
	select {
	case <-approvalCh:
		t.Fatal("stale approval was accepted")
	default:
	}
	select {
	case <-planCh:
		t.Fatal("stale plan response was accepted")
	default:
	}

	submitTestCommand(t, ag, protocol.Command{ID: protocol.CommandID(nextRuntimeID("approval")), SessionID: protocol.SessionID(ag.SessionID()),
		Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: "approval-new", Approve: true}})
	submitTestCommand(t, ag, protocol.Command{ID: protocol.CommandID(nextRuntimeID("plan")), SessionID: protocol.SessionID(ag.SessionID()),
		Type: protocol.CommandApprovePlan, Plan: &protocol.ApprovePlan{PlanID: "plan-new", Approve: true}})
	if ans := <-approvalCh; !ans.approve {
		t.Fatal("matching approval was not accepted")
	}
	if ok := <-planCh; !ok {
		t.Fatal("matching plan response was not accepted")
	}
}

func TestReloadSettingsRevokesRemovedProjectRulesAndHooks(t *testing.T) {
	ag := newRuntimeAgent(t)
	settingsDir := filepath.Join(ag.services.cfg.Workspace, ".ccdp")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	first := `{"permission_mode":"bypassPermissions","always_allow":["Bash:make build"],"hooks":{"PreToolUse":["echo ok"]},"enable_web_tools":false}`
	if err := os.WriteFile(settingsPath, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	// Executable project settings require an explicit, external trust record;
	// use a temp store so this test never touches the user's real trust state.
	trust := config.NewTrustStore(filepath.Join(t.TempDir(), "project-trust.json"))
	project, err := config.LoadProjectSettings(ag.services.cfg.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.AuthorizeProject(ag.services.cfg.Workspace, project); err != nil {
		t.Fatal(err)
	}
	ag.mu.Lock()
	ag.services.trustStore = trust
	ag.mu.Unlock()
	if err := ag.ReloadSettings(); err != nil {
		t.Fatal(err)
	}
	if decision, _ := ag.services.perms.Check("Bash", map[string]any{"command": "make build"}, permissions.KindShell); decision != permissions.DecisionAllow {
		t.Fatalf("project allow rule was not loaded: %s", decision)
	}
	if len(ag.HooksList()[hooks.EventPreToolUse]) != 1 {
		t.Fatal("project hook was not loaded")
	}
	if ag.PermissionMode() != permissions.ModeAcceptEdits {
		t.Fatalf("project attempted to loosen permission policy: %s", ag.PermissionMode())
	}
	if _, ok := ag.services.registry.Get("WebFetch"); ok {
		t.Fatal("web tools remained registered after disabling them")
	}
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ag.ReloadSettings(); err != nil {
		t.Fatal(err)
	}
	if decision, _ := ag.services.perms.Check("Bash", map[string]any{"command": "make build"}, permissions.KindShell); decision != permissions.DecisionAsk {
		t.Fatalf("removed project allow rule remained active: %s", decision)
	}
	if len(ag.HooksList()[hooks.EventPreToolUse]) != 0 {
		t.Fatal("removed project hook remained active")
	}
	if ag.PermissionMode() != permissions.ModeAcceptEdits {
		t.Fatalf("removed project permission settings remained active: %s", ag.PermissionMode())
	}
	if _, ok := ag.services.registry.Get("WebFetch"); !ok {
		t.Fatal("web tools were not restored after removing the override")
	}
}

func TestProjectSettingsLoadedAtStartup(t *testing.T) {
	dir := t.TempDir()
	settingsDir := filepath.Join(dir, ".ccdp")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"),
		[]byte(`{"permission_mode":"acceptEdits","always_deny":["Bash:git push*"],"enable_web_tools":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	trust := config.NewTrustStore(filepath.Join(t.TempDir(), "project-trust.json"))
	project, err := config.LoadProjectSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.AuthorizeProject(dir, project); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.PermissionMode = string(permissions.ModeDefault)
	cfg.Workspace = dir
	cfg.SessionDir = filepath.Join(dir, "sessions")
	cfg.TrustStore = trust
	ag, err := testNew(t, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	if ag.PermissionMode() != permissions.ModeDefault {
		t.Fatalf("untrusted project loosened startup permission mode = %s", ag.PermissionMode())
	}
	if decision, _ := ag.services.perms.Check("Bash", map[string]any{"command": "git push origin main"}, permissions.KindShell); decision != permissions.DecisionDeny {
		t.Fatalf("startup deny rule decision = %s", decision)
	}
	if _, ok := ag.services.registry.Get("WebFetch"); ok {
		t.Fatal("startup web setting was ignored")
	}
}
