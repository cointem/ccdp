package tui

import (
	"testing"

	"ccdp/internal/protocol"
)

func TestWorkspaceCommandsUseTypedRuntimePayloads(t *testing.T) {
	tests := []struct {
		line  string
		check func(t *testing.T, command protocol.Command)
	}{
		{line: "/init", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandInit || command.Init == nil {
				t.Fatalf("init command = %#v", command)
			}
		}},
		{line: "/checkpoint create before release", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandCheckpoint || command.Checkpoint == nil || command.Checkpoint.Action != protocol.CheckpointCreate || command.Checkpoint.Summary != "before release" {
				t.Fatalf("checkpoint command = %#v", command)
			}
		}},
		{line: "/checkpoint restore ck-1", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandCheckpoint || command.Checkpoint == nil || command.Checkpoint.Action != protocol.CheckpointRestore || command.Checkpoint.ID != "ck-1" {
				t.Fatalf("checkpoint command = %#v", command)
			}
		}},
		{line: "/export report.md", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandExport || command.Export == nil || command.Export.Path != "report.md" {
				t.Fatalf("export command = %#v", command)
			}
		}},
		{line: `/apply "plan file.md"`, check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandApply || command.Apply == nil || command.Apply.Path != "plan file.md" {
				t.Fatalf("apply command = %#v", command)
			}
		}},
		{line: "/git status --short", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandExternal || command.External == nil || command.External.Program != "git" || len(command.External.Args) != 2 || command.External.Args[0] != "status" || command.External.Args[1] != "--short" {
				t.Fatalf("git command = %#v", command)
			}
		}},
		{line: "/pr-comments", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandExternal || command.External == nil || command.External.Program != "gh" {
				t.Fatalf("pr-comments command = %#v", command)
			}
		}},
		{line: "/mcp", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandQuery || command.Query == nil || command.Query.Kind != protocol.QueryMCP {
				t.Fatalf("mcp command = %#v", command)
			}
		}},
		{line: "/skills", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandQuery || command.Query == nil || command.Query.Kind != protocol.QuerySkills {
				t.Fatalf("skills command = %#v", command)
			}
		}},
		{line: "/doctor", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandQuery || command.Query == nil || command.Query.Kind != protocol.QueryDoctor {
				t.Fatalf("doctor command = %#v", command)
			}
		}},
		{line: "/sandbox", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandQuery || command.Query == nil || command.Query.Kind != protocol.QueryDoctor {
				t.Fatalf("sandbox diagnostic command = %#v", command)
			}
		}},
		{line: "/add-dir /tmp/shared", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandSetSandboxPolicy || command.SandboxPolicy == nil || len(command.SandboxPolicy.Policy.AdditionalDirectories) != 1 || command.SandboxPolicy.Policy.AdditionalDirectories[0] != "/tmp/shared" {
				t.Fatalf("writable root command = %#v", command)
			}
		}},
		{line: "/add-dir --read-only /tmp/reference", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandSetSandboxPolicy || command.SandboxPolicy == nil || len(command.SandboxPolicy.Policy.AdditionalReadOnlyDirectories) != 1 || command.SandboxPolicy.Policy.AdditionalReadOnlyDirectories[0] != "/tmp/reference" {
				t.Fatalf("read-only root command = %#v", command)
			}
		}},
		{line: "/disallowed-dir /tmp/private", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandSetSandboxPolicy || command.SandboxPolicy == nil || len(command.SandboxPolicy.Policy.DisallowedDirectories) != 1 || command.SandboxPolicy.Policy.DisallowedDirectories[0] != "/tmp/private" {
				t.Fatalf("denied root command = %#v", command)
			}
		}},
		{line: "/config set network-access allow", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandSetSandboxPolicy || command.SandboxPolicy == nil || !command.SandboxPolicy.Policy.NetworkAccess {
				t.Fatalf("network authorization command = %#v", command)
			}
		}},
		{line: "/memory", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandQuery || command.Query == nil || command.Query.Kind != protocol.QueryMemory {
				t.Fatalf("memory command = %#v", command)
			}
		}},
		{line: "/memory clear", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandClearMemory || command.ClearMemory == nil {
				t.Fatalf("memory clear command = %#v", command)
			}
		}},
		{line: "/cd /tmp", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandSetWorkspace || command.Workspace == nil || command.Workspace.Path != "/tmp" {
				t.Fatalf("cd command = %#v", command)
			}
		}},
		{line: "/reload", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandReloadSettings || command.Reload == nil {
				t.Fatalf("reload command = %#v", command)
			}
		}},
		{line: "/save", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandSaveSession || command.SaveSession == nil {
				t.Fatalf("save command = %#v", command)
			}
		}},
		{line: "/trust", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandTrustProject || command.TrustProject == nil || command.TrustProject.Revoke {
				t.Fatalf("trust command = %#v", command)
			}
		}},
		{line: "/trust revoke", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandTrustProject || command.TrustProject == nil || !command.TrustProject.Revoke {
				t.Fatalf("trust revoke command = %#v", command)
			}
		}},
		{line: "/github", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandQuery || command.Query == nil || command.Query.Kind != protocol.QueryGitHub {
				t.Fatalf("github command = %#v", command)
			}
		}},
		{line: "/diff", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandExternal || command.External == nil || command.External.Program != "git" || len(command.External.Args) != 1 || command.External.Args[0] != "diff" {
				t.Fatalf("diff command = %#v", command)
			}
		}},
		{line: "/review", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandRunWorkflow || command.Workflow == nil || command.Workflow.Kind != protocol.WorkflowReview || command.Workflow.Message != "" {
				t.Fatalf("review command = %#v", command)
			}
		}},
		{line: "/commit-push-pr fix bug", check: func(t *testing.T, command protocol.Command) {
			if command.Type != protocol.CommandRunWorkflow || command.Workflow == nil || command.Workflow.Kind != protocol.WorkflowCommitPushPR || command.Workflow.Message != "fix bug" {
				t.Fatalf("commit/push/pr command = %#v", command)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.line, func(t *testing.T) {
			m := sugModel()
			_, cmd := m.runCommand(test.line)
			if cmd == nil {
				t.Fatal("command returned no protocol command")
			}
			msg := cmd()
			receipt, ok := msg.(commandReceiptMsg)
			if !ok {
				t.Fatalf("command result = %T (%#v)", msg, msg)
			}
			if receipt.receipt.Rejected() {
				t.Fatalf("command rejected: %v", receipt.receipt.Error)
			}
			client := m.client.(*recordingClient)
			if len(client.submits) != 1 {
				t.Fatalf("submitted commands = %#v", client.submits)
			}
			test.check(t, client.submits[0])
		})
	}
}
