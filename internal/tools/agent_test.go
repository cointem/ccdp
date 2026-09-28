package tools

import (
	"ccdp/internal/protocol"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type agentToolDirectory struct {
	protocol.SessionDirectory
	row     protocol.ChildSession
	control protocol.AgentControl
}

func (d *agentToolDirectory) ListChildren(context.Context) ([]protocol.ChildSession, error) {
	return []protocol.ChildSession{d.row}, nil
}
func (d *agentToolDirectory) Control(_ context.Context, c protocol.AgentControl) (protocol.RunView, error) {
	d.control = c
	return d.row.Run, nil
}

func TestSpawnAgentDefaultsAndOverrides(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		args := map[string]any{"task": "inspect"}
		role, workspace, contextMode := "worker", "shared", "fresh"
		if explicit {
			role, workspace, contextMode = "explorer", "isolated", "fork"
			args["role"], args["workspace"], args["context"] = role, workspace, contextMode
		}
		var got SpawnAgentRequest
		out, err := NewAgentTool("SpawnAgent").Run(&Context{Context: context.Background(), Args: args, SpawnAgent: func(task SpawnAgentRequest) (protocol.AgentReceipt, error) {
			got = task
			return protocol.AgentReceipt{AgentID: "child", Status: "accepted"}, nil
		}})
		if err != nil || got.Role != role || got.Workspace != workspace || got.Context != contextMode {
			t.Fatalf("%+v %v", got, err)
		}
		var receipt protocol.AgentReceipt
		if json.Unmarshal([]byte(out), &receipt) != nil || receipt.AgentID != "child" || receipt.Status != "accepted" || strings.Contains(out, "internal") || strings.Contains(out, "secret") {
			t.Fatal(out)
		}
	}
}
func TestWaitAgentContract(t *testing.T) {
	if len(NewAgentTool("WaitAgent").Parameters()["properties"].(map[string]any)) != 0 {
		t.Fatal("wait exposes target or timeout")
	}
	for _, reason := range []string{"collaboration", "attention", "user_input", "no_pending_work"} {
		out, err := NewAgentTool("WaitAgent").Run(&Context{Context: context.Background(), WaitAgentEvent: func(context.Context) (string, error) { return reason, nil }})
		if err != nil || out != `{"reason":"`+reason+`"}` {
			t.Fatalf("%s %v", out, err)
		}
	}
}
func TestWaitAgentPreservesCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		if !deadline {
			cancel()
		}
		_, err := NewAgentTool("WaitAgent").Run(&Context{Context: ctx, WaitAgentEvent: func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }})
		cancel()
		want := context.Canceled
		if deadline {
			want = context.DeadlineExceeded
		}
		if !errors.Is(err, want) {
			t.Fatal(err)
		}
	}
}
func TestFollowupAndStopUseStableAgentIdentity(t *testing.T) {
	for _, name := range []string{"FollowupAgent", "StopAgent"} {
		d := &agentToolDirectory{row: protocol.ChildSession{Run: protocol.RunView{Status: "queued"}}}
		args := map[string]any{"agent_id": "child"}
		if name == "FollowupAgent" {
			args["task"] = "next"
		}
		out, err := NewAgentTool(name).Run(&Context{Context: context.Background(), Args: args, Sessions: d, AgentCommandID: "command"})
		if err != nil || d.control.RunID != "" || d.control.SessionID != "child" {
			t.Fatalf("%s %+v %v", out, d.control, err)
		}
		if name == "FollowupAgent" && d.control.Action != "continue" {
			t.Fatal(d.control)
		}
	}
}
func TestSendMessageNeedsNoDirectoryControl(t *testing.T) {
	called := false
	_, err := NewAgentTool("SendMessage").Run(&Context{Context: context.Background(), Args: map[string]any{"agent_id": "parent", "text": "progress"}, SendAgentMessage: func(target, text string) error { called = target == "parent" && text == "progress"; return nil }})
	if err != nil || !called {
		t.Fatalf("%v %v", called, err)
	}
}
func TestAgentToolsRejectOldRunParameters(t *testing.T) {
	for _, name := range []string{"WaitAgent", "FollowupAgent", "StopAgent"} {
		_, err := NewAgentTool(name).Run(&Context{Context: context.Background(), Args: map[string]any{"run_id": "old"}})
		if err == nil || !strings.Contains(err.Error(), "unknown parameter") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
func TestReadAgentDefaultsToResult(t *testing.T) {
	d := &agentToolDirectory{}
	var got protocol.AgentReadRequest
	out, err := NewAgentTool("ReadAgent").Run(&Context{Context: context.Background(), Args: map[string]any{"agent_id": "child"}, Sessions: d, ReadAgent: func(_ context.Context, req protocol.AgentReadRequest, _ int) (string, error) {
		got = req
		return "saved", nil
	}})
	if err != nil || out != "saved" || got.View != "result" || got.AgentID != "child" {
		t.Fatalf("%+v %s %v", got, out, err)
	}
}
func TestListAgentsNeverReturnsReport(t *testing.T) {
	d := &agentToolDirectory{row: protocol.ChildSession{SessionID: "child", Name: "worker", Run: protocol.RunView{ID: "internal", Status: "succeeded", Output: strings.Repeat("report", 20000)}}}
	out, err := NewAgentTool("ListAgents").Run(&Context{Context: context.Background(), Sessions: d})
	if err != nil || strings.Contains(out, "report") || strings.Contains(out, "internal") || !strings.Contains(out, `"outcome":"succeeded"`) {
		t.Fatalf("%s %v", out, err)
	}
}
