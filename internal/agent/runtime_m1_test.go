package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
)

func TestM1StepFreezesModelClientAndConfigAcrossStream(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstBody := make(chan map[string]any, 1)
	firstAuth := make(chan string, 1)
	secondBody := make(chan map[string]any, 1)
	secondAuth := make(chan string, 1)
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		select {
		case firstBody <- body:
		default:
		}
		select {
		case firstAuth <- r.Header.Get("Authorization"):
		default:
		}
		close(firstStarted)
		select {
		case <-releaseFirst:
		case <-r.Context().Done():
			return
		}
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"read-1","function":{"name":"Read","arguments":"{\"file_path\":\"missing.txt\"}"}}]}}]}`)
		writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		writeSSE(w, `[DONE]`)
	}))
	defer firstServer.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode second request: %v", err)
			return
		}
		select {
		case secondBody <- body:
		default:
		}
		select {
		case secondAuth <- r.Header.Get("Authorization"):
		default:
		}
		writeSSE(w, `{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)
		writeSSE(w, `[DONE]`)
	}))
	defer secondServer.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.BaseURL = firstServer.URL
	cfg.APIKey = "key-a"
	cfg.Model = "model-a"
	cfg.PermissionMode = string(permissions.ModeBypass)
	cfg.MaxTurns = 3
	cfg.Providers = map[string]config.ProviderConfig{
		"provider-a": {BaseURL: firstServer.URL, APIKey: "key-a", Models: []string{"model-a"}},
		"provider-b": {BaseURL: secondServer.URL, APIKey: "key-b", Models: []string{"model-b"}},
	}
	ag, err := New(&cfg, make(chan Event, 256))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	input := protocol.NewSubmitInput("input-1", protocol.SessionID(ag.SessionID()), "input-1", "inspect", protocol.InputSteer)
	if receipt, err := ag.Submit(context.Background(), input); err != nil || receipt.Rejected() {
		t.Fatalf("submit: receipt=%+v err=%v", receipt, err)
	}
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first provider step did not start")
	}
	select {
	case body := <-firstBody:
		if got, _ := body["model"].(string); got != "model-a" {
			t.Fatalf("first step model = %q, want model-a", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request body was not captured")
	}
	if got := <-firstAuth; got != "Bearer key-a" {
		t.Fatalf("first provider authorization = %q, want key-a", got)
	}

	model := protocol.NewSetModel("model-command", protocol.SessionID(ag.SessionID()), "model-b")
	receipt, err := ag.Submit(context.Background(), model)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != protocol.ReceiptApplied {
		t.Fatalf("model change while streaming = %s, want applied", receipt.Status)
	}
	// Immediate application moves the live binding now, but the in-flight first
	// step already captured its own model-a client/config at the step boundary,
	// so the first request it is streaming is undisturbed (verified above) and
	// the change applied immediately.
	ag.mu.Lock()
	if ag.activeBinding.model != "model-b" {
		ag.mu.Unlock()
		t.Fatalf("model did not apply immediately: active=%q", ag.activeBinding.model)
	}
	ag.mu.Unlock()
	close(releaseFirst)

	select {
	case body := <-secondBody:
		if got, _ := body["model"].(string); got != "model-b" {
			t.Fatalf("next step model = %q, want model-b", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second provider step did not start")
	}
	if got := <-secondAuth; got != "Bearer key-b" {
		t.Fatalf("second provider authorization = %q, want key-b", got)
	}

	// The second request must be built from the same frozen binding: the client
	// used to send it is the B client, and no A model may be paired with it.
	ag.mu.Lock()
	if ag.activeBinding.model != "model-b" || ag.activeBinding.client == nil || ag.activeBinding.client.Name() != "model-b" {
		ag.mu.Unlock()
		t.Fatalf("active binding after barrier = %+v", ag.activeBinding)
	}
	ag.mu.Unlock()
}

func TestM1CapturedStepRemainsFrozenAfterBindingSwitch(t *testing.T) {
	dir := t.TempDir()
	first := httptest.NewServer(http.NotFoundHandler())
	defer first.Close()
	second := httptest.NewServer(http.NotFoundHandler())
	defer second.Close()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.BaseURL = first.URL
	cfg.APIKey = "key-a"
	cfg.Model = "model-a"
	cfg.Providers = map[string]config.ProviderConfig{
		"a": {BaseURL: first.URL, APIKey: "key-a", Models: []string{"model-a"}},
		"b": {BaseURL: second.URL, APIKey: "key-b", Models: []string{"model-b"}},
	}
	ag, err := New(&cfg, make(chan Event, 16))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	ag.mu.Lock()
	ag.cfg.SystemPrompt = "before-switch"
	ag.mu.Unlock()
	old := ag.beginStep()

	switchCommand := protocol.NewSetModel("switch", protocol.SessionID(ag.SessionID()), "model-b")
	if receipt := ag.applyCommand(switchCommand); receipt.Rejected() {
		t.Fatalf("model switch rejected: %+v", receipt)
	}
	ag.mu.Lock()
	ag.cfg.SystemPrompt = "after-switch"
	ag.mu.Unlock()
	oldReq := ag.buildRequestForStep(old)
	system, _ := oldReq.Messages[0].Content.(string)
	if oldReq.Model != "model-a" || old.binding.model != "model-a" || old.binding.endpoint != first.URL || old.binding.client.Name() != "model-a" {
		t.Fatalf("old step binding changed: model=%q binding=%+v", oldReq.Model, old.binding)
	}
	if !strings.Contains(system, "before-switch") || strings.Contains(system, "after-switch") {
		t.Fatalf("old step used mutable config: %q", system)
	}
	newStep := ag.beginStep()
	if newStep.binding.model != "model-b" || newStep.binding.endpoint != second.URL || newStep.binding.client.Name() != "model-b" {
		t.Fatalf("new step did not adopt B binding: %+v", newStep.binding)
	}
}

func TestM1SnapshotProjectsContextUsageAndStablePlanVersion(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.mu.Lock()
	ag.cfg.ContextWindow = 8192
	ag.history = []messages.Message{{Role: messages.RoleUser, Content: strings.Repeat("context", 32)}}
	ag.pendingPlan = &PlanRequest{ID: "plan-stable", Plan: "keep this plan"}
	ag.settingsRev = 7
	ag.mu.Unlock()

	view, err := ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.ContextUsedTokens <= 0 {
		t.Fatalf("context usage was not projected: %+v", view)
	}
	if view.Settings.ContextWindow != 8192 {
		t.Fatalf("context window = %d, want 8192", view.Settings.ContextWindow)
	}
	if view.Plan == nil || view.Plan.Version != planViewVersion {
		t.Fatalf("plan snapshot = %+v, want stable version %d", view.Plan, planViewVersion)
	}

	// Settings revisions may advance while a plan is pending; the plan token is
	// tied to its immutable ID, not reused as a settings revision.
	ag.mu.Lock()
	ag.settingsRev = 99
	ag.mu.Unlock()
	view, err = ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Plan == nil || view.Plan.Version != planViewVersion {
		t.Fatalf("plan version changed with settings: %+v", view.Plan)
	}
}

func TestM1ModeAndSandboxOnlyPreserveExistingRules(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.BaseURL = "http://127.0.0.1:1"
	cfg.PermissionMode = string(permissions.ModeDefault)
	cfg.AlwaysAllow = []string{"Read:*"}
	cfg.AlwaysDeny = []string{"Bash:rm *"}
	cfg.SandboxMode = string(sandbox.ModeStrict)
	cfg.SandboxAllowNetwork = true
	cfg.AdditionalDirectories = []string{dir + "/extra"}
	cfg.DisallowedDirectories = []string{dir + "/blocked"}
	ag, err := New(&cfg, make(chan Event, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	modeCmd := protocol.Command{ID: "mode-only-legacy", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeAcceptEdits)}}}
	if receipt := ag.applyCommand(modeCmd); receipt.Rejected() {
		t.Fatalf("mode-only command rejected: %+v", receipt)
	}
	ag.mu.Lock()
	if strings.Join(ag.cfg.AlwaysDeny, "\n") != "Bash:rm *" || strings.Join(ag.cfg.AlwaysAllow, "\n") != "Read:*" {
		ag.mu.Unlock()
		t.Fatalf("mode-only changed rules: allow=%v deny=%v", ag.cfg.AlwaysAllow, ag.cfg.AlwaysDeny)
	}
	ag.mu.Unlock()

	sandboxCmd := protocol.Command{ID: "sandbox-only-legacy", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandSetSandboxPolicy,
		SandboxPolicy: &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{Mode: string(sandbox.ModeConfine)}}}
	if receipt := ag.applyCommand(sandboxCmd); receipt.Rejected() {
		t.Fatalf("sandbox-only command rejected: %+v", receipt)
	}
	ag.mu.Lock()
	if !ag.cfg.SandboxAllowNetwork || len(ag.cfg.AdditionalDirectories) != 1 || len(ag.cfg.DisallowedDirectories) != 1 {
		t.Fatalf("sandbox-only changed policy: network=%v additional=%v disallowed=%v", ag.cfg.SandboxAllowNetwork, ag.cfg.AdditionalDirectories, ag.cfg.DisallowedDirectories)
	}
	ag.mu.Unlock()

	// Canonical mode-only policy commands use nil rule slices and must retain
	// the existing rule set as well.
	canonical := protocol.Command{ID: "mode-only", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeDefault)}}}
	if receipt := ag.applyCommand(canonical); receipt.Rejected() {
		t.Fatalf("canonical mode-only command rejected: %+v", receipt)
	}
	ag.mu.Lock()
	if len(ag.cfg.AlwaysDeny) != 1 || len(ag.cfg.AlwaysAllow) != 1 {
		ag.mu.Unlock()
		t.Fatalf("canonical mode-only changed rules: allow=%v deny=%v", ag.cfg.AlwaysAllow, ag.cfg.AlwaysDeny)
	}
	ag.mu.Unlock()
	canonicalSandbox := protocol.Command{ID: "sandbox-only", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandSetSandboxPolicy,
		SandboxPolicy: &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{Mode: string(sandbox.ModeStrict)}}}
	if receipt := ag.applyCommand(canonicalSandbox); receipt.Rejected() {
		t.Fatalf("canonical sandbox-only command rejected: %+v", receipt)
	}
	ag.mu.Lock()
	if !ag.cfg.SandboxAllowNetwork || len(ag.cfg.AdditionalDirectories) != 1 || len(ag.cfg.DisallowedDirectories) != 1 {
		ag.mu.Unlock()
		t.Fatalf("canonical sandbox-only changed policy: network=%v additional=%v disallowed=%v", ag.cfg.SandboxAllowNetwork, ag.cfg.AdditionalDirectories, ag.cfg.DisallowedDirectories)
	}
	ag.mu.Unlock()
}

// TestM1SafetyChangesApplyImmediatelyWhileBusy covers the discrete-decision lane:
// execution (plan) mode, permission policy and sandbox policy land on live state
// at once even while a turn runs, because each provider request froze its own
// config and each gate reads the mode atomically. Nothing is staged.
func TestM1SafetyChangesApplyImmediatelyWhileBusy(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.mu.Lock()
	ag.busy = true
	ag.mu.Unlock()
	session := protocol.SessionID(ag.SessionID())
	commands := []protocol.Command{
		{ID: "busy-execution", SessionID: session, Type: protocol.CommandSetExecutionMode, ExecutionMode: &protocol.SetExecutionMode{Mode: protocol.ExecutionModePlan}},
		{ID: "busy-permission", SessionID: session, Type: protocol.CommandSetPermissionPolicy, PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeBypass)}}},
		{ID: "busy-sandbox", SessionID: session, Type: protocol.CommandSetSandboxPolicy, SandboxPolicy: &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{Mode: string(sandbox.ModeNone)}}},
	}
	for _, command := range commands {
		receipt := ag.applyCommand(command)
		if receipt.Rejected() || receipt.Status != protocol.ReceiptApplied {
			t.Fatalf("busy safety command %s = %+v, want applied", command.ID, receipt)
		}
	}
	// The discrete lane mutates live state immediately.
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if !ag.planMode {
		t.Fatal("plan mode did not apply immediately")
	}
	if ag.cfg.PermissionMode != string(permissions.ModeBypass) {
		t.Fatalf("permission mode did not apply immediately: %s", ag.cfg.PermissionMode)
	}
	if ag.cfg.SandboxMode != string(sandbox.ModeNone) {
		t.Fatalf("sandbox mode did not apply immediately: %s", ag.cfg.SandboxMode)
	}
}

func TestM1PlanApprovalRestoresPermissionAndRejectStaysDrafting(t *testing.T) {
	ag := newRuntimeAgent(t)
	modeCommand := protocol.Command{ID: "plan-mode-permission", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: protocol.PermissionPolicy{Mode: string(permissions.ModeAcceptEdits)}}}
	if receipt := ag.applyCommand(modeCommand); receipt.Rejected() {
		t.Fatalf("permission mode command rejected: %+v", receipt)
	}
	if got := ag.PermissionMode(); got != permissions.ModeAcceptEdits {
		t.Fatalf("permission mode did not apply: %s", got)
	}

	ag.setExecutionMode(true)
	ag.mu.Lock()
	ag.pendingPlan = &PlanRequest{ID: "plan-approve", Plan: "do it"}
	ag.planResp = make(chan bool, 1)
	ag.mu.Unlock()
	approve := protocol.Command{ID: "approve", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandApprovePlan,
		Plan: &protocol.ApprovePlan{PlanID: "plan-approve", Approve: true}}
	if receipt := ag.applyCommand(approve); receipt.Rejected() {
		t.Fatalf("plan approval rejected: %+v", receipt)
	}
	if ag.PlanMode() || ag.PermissionMode() != permissions.ModeAcceptEdits {
		t.Fatalf("approval did not restore permission state: plan=%v mode=%s", ag.PlanMode(), ag.PermissionMode())
	}

	ag.setExecutionMode(true)
	ag.mu.Lock()
	ag.pendingPlan = &PlanRequest{ID: "plan-reject", Plan: "revise"}
	ag.planResp = make(chan bool, 1)
	ag.mu.Unlock()
	reject := protocol.Command{ID: "reject", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandApprovePlan,
		Plan: &protocol.ApprovePlan{PlanID: "plan-reject", Approve: false}}
	if receipt := ag.applyCommand(reject); receipt.Rejected() {
		t.Fatalf("plan rejection rejected: %+v", receipt)
	}
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if !ag.planMode || ag.workflow != protocol.WorkflowDrafting || ag.perms.CurrentMode() != permissions.ModeAcceptEdits {
		t.Fatalf("rejection changed execution/permission state: plan=%v workflow=%s mode=%s", ag.planMode, ag.workflow, ag.perms.CurrentMode())
	}
}

func TestM1CompactInterruptDoesNotDrainQueuedInput(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.BaseURL = server.URL
	cfg.APIKey = "test"
	cfg.PermissionMode = string(permissions.ModeBypass)
	cfg.KeepAfterCompact = 4
	ag, err := New(&cfg, make(chan Event, 128))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	for i := 0; i < 12; i++ {
		ag.appendHistory(messages.Message{Role: messages.RoleUser, Content: "history"})
	}
	compact := protocol.Command{ID: "compact", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandCompact}
	if receipt, err := ag.Submit(context.Background(), compact); err != nil || receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("compact admission: receipt=%+v err=%v", receipt, err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("compaction provider call did not start")
	}
	queued := protocol.NewSubmitInput("queued-command", protocol.SessionID(ag.SessionID()), "queued-input", "must wait", protocol.InputFollowup)
	if receipt, err := ag.Submit(context.Background(), queued); err != nil || receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("queued input admission: receipt=%+v err=%v", receipt, err)
	}
	interrupt := protocol.Command{ID: "interrupt", SessionID: protocol.SessionID(ag.SessionID()), Type: protocol.CommandInterrupt}
	if receipt, err := ag.Submit(context.Background(), interrupt); err != nil || receipt.Rejected() {
		t.Fatalf("interrupt admission: receipt=%+v err=%v", receipt, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ag.mu.Lock()
		busy, pending, phase := ag.busy, len(ag.pendingMsgs), ag.phase
		ag.mu.Unlock()
		if !busy && pending == 1 && phase == protocol.PhaseIdle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	ag.mu.Lock()
	defer ag.mu.Unlock()
	t.Fatalf("interrupted compaction drained/ran queue: busy=%v pending=%d phase=%s", ag.busy, len(ag.pendingMsgs), ag.phase)
}

func TestM1SubmitPersistsInputBeforeQueueAdmission(t *testing.T) {
	ag := newRuntimeAgent(t)
	ag.mu.Lock()
	ag.busy = true
	ag.mu.Unlock()

	cmd := protocol.NewSubmitInput("durable-command", protocol.SessionID(ag.SessionID()), "durable-input", "must survive restart", protocol.InputFollowup)
	receipt := ag.applyCommand(cmd)
	if receipt.Rejected() {
		t.Fatalf("durable submit rejected: %+v", receipt)
	}
	snapshot, err := LoadSession(ag.SessionDir(), ag.SessionID())
	if err != nil {
		t.Fatalf("load durable queue: %v", err)
	}
	if len(snapshot.Pending) != 1 || snapshot.Pending[0] != "must survive restart" {
		t.Fatalf("durable pending inbox = %v", snapshot.Pending)
	}
	if receipt.Revision.LogSeq == 0 {
		t.Fatalf("receipt did not expose durable cursor: %+v", receipt)
	}
}

func TestM1DurableInputResumeDoesNotReexecuteDeliveredID(t *testing.T) {
	started := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-started:
		default:
			close(started)
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.BaseURL = server.URL
	cfg.APIKey = "resume-key"
	cfg.PermissionMode = string(permissions.ModeBypass)
	first, err := New(&cfg, make(chan Event, 256))
	if err != nil {
		t.Fatal(err)
	}

	sessionID := protocol.SessionID(first.SessionID())
	initial := protocol.NewSubmitInput("resume-command-a", sessionID, "resume-input-a", "same text", protocol.InputSteer)
	if receipt, err := first.Submit(context.Background(), initial); err != nil || receipt.Rejected() {
		t.Fatalf("initial submit: receipt=%+v err=%v", receipt, err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		first.Close()
		t.Fatal("blocking provider did not start")
	}

	steer := protocol.NewSubmitInput("resume-command-b", sessionID, "resume-input-b", "same text", protocol.InputSteer)
	followup := protocol.NewSubmitInput("resume-command-c", sessionID, "resume-input-c", "same text", protocol.InputFollowup)
	for _, cmd := range []protocol.Command{steer, followup} {
		if receipt, err := first.Submit(context.Background(), cmd); err != nil || receipt.Rejected() {
			t.Fatalf("queued submit: receipt=%+v err=%v", receipt, err)
		}
	}

	interrupt := protocol.Command{ID: "resume-interrupt", SessionID: sessionID, Type: protocol.CommandInterrupt}
	if receipt, err := first.Submit(context.Background(), interrupt); err != nil || receipt.Rejected() {
		t.Fatalf("interrupt: receipt=%+v err=%v", receipt, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, snapshotErr := first.Snapshot(context.Background())
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if !view.Busy && len(view.PendingInputs) == 2 && view.LastTurn != nil && view.LastTurn.Status == protocol.TurnCancelled {
			break
		}
		time.Sleep(time.Millisecond)
	}
	view, err := first.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Busy || len(view.PendingInputs) != 2 {
		t.Fatalf("interrupt drained queue: busy=%v pending=%+v", view.Busy, view.PendingInputs)
	}

	first.Close()
	snap, err := LoadSession(cfg.SessionDir, string(sessionID))
	if err != nil {
		t.Fatalf("load after close: %v", err)
	}
	if len(snap.PendingInputs) != 2 || snap.PendingInputs[0].Strategy != protocol.InputSteer || snap.PendingInputs[1].Strategy != protocol.InputFollowup {
		t.Fatalf("durable typed pending inputs = %+v", snap.PendingInputs)
	}
	if snap.PendingInputs[0].CreatedAt.IsZero() || snap.PendingInputs[1].CreatedAt.IsZero() {
		t.Fatalf("durable input timestamps missing: %+v", snap.PendingInputs)
	}

	resumed, err := Resume(&cfg, snap, make(chan Event, 256))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer resumed.Close()
	retry := protocol.NewSubmitInput("resume-command-retry", protocol.SessionID(resumed.SessionID()), "resume-input-a", "same text", protocol.InputSteer)
	receipt, err := resumed.Submit(context.Background(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Rejected() || receipt.Status != protocol.ReceiptApplied {
		t.Fatalf("retry of delivered input = %+v", receipt)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("retry re-executed provider: request count=%d", got)
	}
}

func TestM1SubmitStoreFailureDoesNotStartProvider(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected provider request", http.StatusInternalServerError)
	}))
	defer server.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace = dir
	cfg.SessionDir = dir + "/sessions"
	cfg.BaseURL = server.URL
	cfg.APIKey = "failure-key"
	cfg.PermissionMode = string(permissions.ModeBypass)
	ag, err := New(&cfg, make(chan Event, 64))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	p := ag.persistenceHandle()
	if p == nil {
		t.Fatal("persistence missing")
	}
	p.mu.Lock()
	underlying := p.store
	p.store = failingCommitStore{Store: underlying}
	p.mu.Unlock()
	before, err := ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cmd := protocol.NewSubmitInput("failed-store-command", protocol.SessionID(ag.SessionID()), "failed-store-input", "must not run", protocol.InputSteer)
	receipt, err := ag.Submit(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Rejected() || receipt.Error == nil {
		t.Fatalf("failing-store receipt = %+v", receipt)
	}
	after, err := ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after.History) != len(before.History) || len(after.PendingInputs) != len(before.PendingInputs) || after.Busy != before.Busy {
		t.Fatalf("failed Submit changed snapshot: before=%+v after=%+v", before, after)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("provider ran after failed durable admission: %d requests", got)
	}
}

func TestM1HistoryCommandsCommitBeforeSwap(t *testing.T) {
	ag := newRuntimeAgent(t)
	sessionID := protocol.SessionID(ag.SessionID())

	// An empty clear is a valid candidate projection and must not panic or
	// fabricate a history entry.
	clearEmpty := protocol.Command{ID: "clear-empty", SessionID: sessionID, Type: protocol.CommandClearConversation}
	if receipt := ag.applyCommand(clearEmpty); receipt.Rejected() {
		t.Fatalf("empty clear rejected: %+v", receipt)
	}
	view, err := ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.History) != 0 {
		t.Fatalf("empty clear history = %+v", view.History)
	}

	ag.appendHistory(
		messages.Message{Role: messages.RoleUser, Content: "A"},
		messages.Message{Role: messages.RoleAssistant, Content: "answer A"},
	)
	if err := ag.Save(); err != nil {
		t.Fatalf("save A: %v", err)
	}

	clearA := protocol.Command{ID: "clear-a", SessionID: sessionID, Type: protocol.CommandClearConversation}
	if receipt := ag.applyCommand(clearA); receipt.Rejected() {
		t.Fatalf("clear A rejected: %+v", receipt)
	}
	if snapshot, err := LoadSession(ag.SessionDir(), ag.SessionID()); err != nil {
		t.Fatalf("load after clear A: %v", err)
	} else if len(snapshot.History) != 0 {
		t.Fatalf("durable clear history = %+v", snapshot.History)
	}

	ag.appendHistory(
		messages.Message{Role: messages.RoleUser, Content: "A"},
		messages.Message{Role: messages.RoleAssistant, Content: "answer A"},
	)
	if err := ag.Save(); err != nil {
		t.Fatalf("save A again: %v", err)
	}
	ag.appendHistory(
		messages.Message{Role: messages.RoleUser, Content: "B"},
		messages.Message{Role: messages.RoleAssistant, Content: "answer B"},
	)
	if err := ag.Save(); err != nil {
		t.Fatalf("save B: %v", err)
	}

	// The projection moves A → B → A. The final remove must commit first and
	// only then replace the in-memory history.
	removeB := protocol.Command{ID: "remove-b", SessionID: sessionID, Type: protocol.CommandRemoveMessages,
		Remove: &protocol.RemoveMessages{Count: 2}}
	if receipt := ag.applyCommand(removeB); receipt.Rejected() {
		t.Fatalf("remove B rejected: %+v", receipt)
	}
	view, err = ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.History) != 2 || view.History[0].Content != "A" || view.History[1].Content != "answer A" {
		t.Fatalf("final A history = %+v", view.History)
	}
	snapshot, err := LoadSession(ag.SessionDir(), ag.SessionID())
	if err != nil {
		t.Fatalf("load final A: %v", err)
	}
	if len(snapshot.History) != 2 || snapshot.History[0].Content != "A" || snapshot.History[1].Content != "answer A" {
		t.Fatalf("durable final A history = %+v", snapshot.History)
	}

	// Once the writer is unavailable, the command is rejected and the live
	// projection remains the previously confirmed A history.
	ag.mu.Lock()
	ag.tokenBaseline.promptTokens = 123
	ag.tokenBaseline.historyLen = len(ag.history)
	ag.mu.Unlock()
	ag.closePersistence()
	rewind := protocol.Command{ID: "rewind-failed", SessionID: sessionID, Type: protocol.CommandRewindConversation,
		Rewind: &protocol.RewindConversation{Count: 1}}
	receipt := ag.applyCommand(rewind)
	if !receipt.Rejected() || receipt.Error == nil || receipt.Error.Code != protocol.ErrorInternal {
		t.Fatalf("failed durable rewind receipt = %+v", receipt)
	}
	view, err = ag.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.History) != 2 || view.History[0].Content != "A" || view.History[1].Content != "answer A" {
		t.Fatalf("failed rewind mutated history = %+v", view.History)
	}
	ag.mu.Lock()
	if ag.tokenBaseline.promptTokens != 123 || ag.tokenBaseline.historyLen != 2 {
		ag.mu.Unlock()
		t.Fatalf("failed rewind mutated token baseline: %+v", ag.tokenBaseline)
	}
	ag.mu.Unlock()
}

func writeSSE(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: " + payload + "\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
