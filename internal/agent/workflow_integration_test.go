package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// workflowTestSetup runs the real runtime workflow worker against commands
// found on a test-only PATH. The scripts never touch a repository or a
// network; they only append their argv and print bounded markers.
type workflowTestSetup struct {
	ag  *Agent
	log string
}

func newWorkflowTestSetup(t *testing.T, permission permissions.Mode, deny []string, gitScript, ghScript string) workflowTestSetup {
	t.Helper()
	return newWorkflowTestSetupAt(t, t.TempDir(), permission, deny, "", gitScript, ghScript)
}

func newWorkflowTestSetupAt(t *testing.T, workspace string, permission permissions.Mode, deny []string, ready, gitScript, ghScript string) workflowTestSetup {
	t.Helper()
	fakeBin := filepath.Join(workspace, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(workspace, "workflow.log")
	writeWorkflowExecutable(t, filepath.Join(fakeBin, "git"), gitScript)
	writeWorkflowExecutable(t, filepath.Join(fakeBin, "gh"), ghScript)

	cfg := config.Default()
	cfg.Workspace = workspace
	cfg.SessionDir = filepath.Join(workspace, "sessions")
	cfg.BaseURL = "http://127.0.0.1:1/unreachable"
	cfg.APIKey = "workflow-test-key"
	cfg.PermissionMode = string(permission)
	cfg.AlwaysDeny = append([]string(nil), deny...)
	cfg.SandboxMode = "none"
	cfg.EnableGuardian = config.BoolPtr(false)
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.MCPServers = nil

	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CCDP_WORKFLOW_LOG", logPath)
	if ready != "" {
		t.Setenv("CCDP_WORKFLOW_READY", ready)
	}

	events := make(chan Event, 256)
	ag, err := New(&cfg, events)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Cleanup is LIFO: close the runtime before its legacy event channel.
	t.Cleanup(func() { close(events) })
	t.Cleanup(func() { _ = ag.CloseContext(context.Background()) })
	return workflowTestSetup{ag: ag, log: logPath}
}

func writeWorkflowExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write fake executable %s: %v", path, err)
	}
}

func workflowCommand(a *Agent, id protocol.CommandID, kind protocol.WorkflowKind, message string) protocol.Command {
	return protocol.NewRunWorkflow(id, protocol.SessionID(a.SessionID()), kind, message)
}

func submitWorkflow(t *testing.T, a *Agent, cmd protocol.Command) {
	t.Helper()
	receipt, err := a.Submit(context.Background(), cmd)
	if err != nil {
		t.Fatalf("submit %s: %v", cmd.ID, err)
	}
	if receipt.Status != protocol.ReceiptScheduled {
		t.Fatalf("submit %s receipt=%+v, want scheduled", cmd.ID, receipt)
	}
}

type workflowFacts struct {
	scheduled []session.CommandScheduled
	completed []session.CommandCompleted
}

func readWorkflowFacts(t *testing.T, a *Agent, id protocol.CommandID) workflowFacts {
	t.Helper()
	p := a.persistenceHandle()
	if p == nil {
		t.Fatal("workflow test requires session persistence")
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		t.Fatalf("read workflow facts: %v", err)
	}
	var facts workflowFacts
	for _, record := range records {
		switch event := record.Event.(type) {
		case session.CommandScheduled:
			if event.CommandID == string(id) {
				facts.scheduled = append(facts.scheduled, event)
			}
		case *session.CommandScheduled:
			if event != nil && event.CommandID == string(id) {
				facts.scheduled = append(facts.scheduled, *event)
			}
		case session.CommandCompleted:
			if event.CommandID == string(id) {
				facts.completed = append(facts.completed, event)
			}
		case *session.CommandCompleted:
			if event != nil && event.CommandID == string(id) {
				facts.completed = append(facts.completed, *event)
			}
		}
	}
	return facts
}

func waitWorkflowCompleted(t *testing.T, a *Agent, id protocol.CommandID) session.CommandCompleted {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		facts := readWorkflowFacts(t, a, id)
		if len(facts.completed) > 0 {
			if len(facts.scheduled) != 1 || len(facts.completed) != 1 {
				t.Fatalf("workflow %s durable boundary scheduled=%d completed=%d", id, len(facts.scheduled), len(facts.completed))
			}
			return facts.completed[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("workflow %s did not complete", id)
	return session.CommandCompleted{}
}

func readWorkflowLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read workflow log: %v", err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func readWorkflowOutput(t *testing.T, setup workflowTestSetup, completed session.CommandCompleted) string {
	t.Helper()
	if completed.Output == nil {
		t.Fatal("workflow completion has no output artifact")
	}
	artifacts := setup.ag.persistenceHandle().Artifacts()
	if artifacts == nil {
		t.Fatal("workflow completion has no artifact store")
	}
	data, err := artifacts.Read(*completed.Output, 1<<20)
	if err != nil {
		t.Fatalf("read workflow output artifact: %v", err)
	}
	return string(data)
}

func TestCommandRunWorkflowCommitPushPROrdersAllStepsAndPersistsOneBoundary(t *testing.T) {
	setup := newWorkflowTestSetup(t, permissions.ModeBypass, nil,
		`printf 'git %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"
case "$1" in
add) printf 'added\n' ;;
commit) printf 'committed\n' ;;
push) printf 'pushed\n' ;;
diff) printf 'diff\n' ;;
esac`,
		`printf 'gh %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"
printf 'pr-created\n'`)
	cmd := workflowCommand(setup.ag, "workflow-success", protocol.WorkflowCommitPushPR, "ship it")
	submitWorkflow(t, setup.ag, cmd)
	completed := waitWorkflowCompleted(t, setup.ag, cmd.ID)
	if completed.Outcome != "success" {
		t.Fatalf("workflow completion=%+v, want success", completed)
	}
	got := readWorkflowLog(t, setup.log)
	want := []string{
		"git add -A",
		"git commit -m ship it",
		"git push -u origin HEAD",
		"gh pr create --fill",
	}
	if len(got) != len(want) {
		t.Fatalf("workflow argv log=%q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("workflow step[%d]=%q, want %q (full log %q)", i, got[i], want[i], got)
		}
	}
	output := readWorkflowOutput(t, setup, completed)
	for _, marker := range []string{"added", "committed", "pushed", "pr-created"} {
		if !strings.Contains(output, marker) {
			t.Fatalf("workflow output=%q missing %q", output, marker)
		}
	}
}

func TestCommandRunWorkflowCommitFailureStopsLaterSteps(t *testing.T) {
	setup := newWorkflowTestSetup(t, permissions.ModeBypass, nil,
		`printf 'git %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"
case "$1" in
add) printf 'added\n' ;;
commit) printf 'commit-failed\n' ; exit 17 ;;
push) printf 'pushed\n' ;;
esac`,
		`printf 'gh %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"
printf 'pr-created\n'`)
	cmd := workflowCommand(setup.ag, "workflow-commit-fails", protocol.WorkflowCommitPushPR, "stop here")
	submitWorkflow(t, setup.ag, cmd)
	completed := waitWorkflowCompleted(t, setup.ag, cmd.ID)
	if completed.Outcome != "error" || !strings.Contains(completed.Report, "git-commit") {
		t.Fatalf("failure completion=%+v, want git-commit error", completed)
	}
	got := readWorkflowLog(t, setup.log)
	want := []string{"git add -A", "git commit -m stop here"}
	if len(got) != len(want) {
		t.Fatalf("failed workflow log=%q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("failed workflow step[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestCommandRunWorkflowInterruptStopsFutureSteps(t *testing.T) {
	workspace := t.TempDir()
	ready := filepath.Join(workspace, "ready")
	setup := newWorkflowTestSetupAt(t, workspace, permissions.ModeBypass, nil, ready,
		`printf 'git %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"
if [ "$1" = add ]; then
  : > "$CCDP_WORKFLOW_READY"
  while :; do sleep 1; done
fi`,
		`printf 'gh %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"
printf 'pr-created\n'`)
	cmd := workflowCommand(setup.ag, "workflow-interrupt", protocol.WorkflowCommitPushPR, "interrupt me")
	submitWorkflow(t, setup.ag, cmd)
	waitForWorkflowFile(t, ready)
	interrupt := protocol.Command{
		ID: "workflow-interrupt-command", SessionID: protocol.SessionID(setup.ag.SessionID()),
		Type: protocol.CommandInterrupt,
	}
	receipt, err := setup.ag.Submit(context.Background(), interrupt)
	if err != nil || receipt.Rejected() {
		t.Fatalf("interrupt receipt=%+v err=%v", receipt, err)
	}
	completed := waitWorkflowCompleted(t, setup.ag, cmd.ID)
	if completed.Outcome != "error" {
		t.Fatalf("interrupt completion=%+v, want cancellation error", completed)
	}
	got := readWorkflowLog(t, setup.log)
	if len(got) != 1 || got[0] != "git add -A" {
		t.Fatalf("interrupt continued future steps: log=%q", got)
	}
}

func waitForWorkflowFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("workflow did not reach %s", path)
}

func TestCommandRunWorkflowDeniedAndPlanDoNotMutate(t *testing.T) {
	t.Run("hard deny", func(t *testing.T) {
		setup := newWorkflowTestSetup(t, permissions.ModeBypass, []string{"Bash"},
			`printf 'git %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"`,
			`printf 'gh %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"`)
		cmd := workflowCommand(setup.ag, "workflow-denied", protocol.WorkflowCommitPushPR, "must not run")
		submitWorkflow(t, setup.ag, cmd)
		completed := waitWorkflowCompleted(t, setup.ag, cmd.ID)
		if completed.Outcome != "error" || !strings.Contains(completed.Report, "always_deny") {
			t.Fatalf("denied completion=%+v", completed)
		}
		if got := readWorkflowLog(t, setup.log); len(got) != 0 {
			t.Fatalf("hard-denied workflow mutated: %q", got)
		}
	})

	t.Run("plan", func(t *testing.T) {
		setup := newWorkflowTestSetup(t, permissions.ModePlan, nil,
			`printf 'git %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"`,
			`printf 'gh %s\n' "$*" >> "$CCDP_WORKFLOW_LOG"`)
		cmd := workflowCommand(setup.ag, "workflow-plan", protocol.WorkflowCommitPushPR, "must not run")
		receipt, err := setup.ag.Submit(context.Background(), cmd)
		if err != nil || !receipt.Rejected() {
			t.Fatalf("plan receipt=%+v err=%v, want rejection", receipt, err)
		}
		if receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidState {
			t.Fatalf("plan receipt error=%+v, want invalid state", receipt.Error)
		}
		if facts := readWorkflowFacts(t, setup.ag, cmd.ID); len(facts.scheduled) != 0 || len(facts.completed) != 0 {
			t.Fatalf("plan workflow persisted operation facts: %+v", facts)
		}
		if got := readWorkflowLog(t, setup.log); len(got) != 0 {
			t.Fatalf("plan workflow mutated: %q", got)
		}
	})
}
