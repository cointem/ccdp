package protocol

import (
	"strings"
	"testing"
)

func TestNormalizeRejectsMismatchedAndMultiplePayloads(t *testing.T) {
	session := SessionID("s")
	cases := []Command{
		{ID: "wrong", SessionID: session, Type: CommandSetModel, Input: &SubmitInput{Text: "x"}},
		{ID: "multiple", SessionID: session, Type: CommandSetModel, Model: &SetModel{Model: "m"}, Input: &SubmitInput{Text: "x"}},
		{ID: "scalar", SessionID: session, Type: CommandInterrupt, Model: &SetModel{Model: "m"}},
	}
	for _, command := range cases {
		if _, err := command.Normalize(); err == nil {
			t.Errorf("Normalize(%s) accepted mismatched payload", command.ID)
		}
	}
}

func TestNormalizeDefaultsStrategyWithoutMutatingCaller(t *testing.T) {
	input := &SubmitInput{ID: "input", Text: "hello"}
	command := Command{ID: "command", SessionID: "session", Type: CommandSubmitInput, Input: input}
	normalized, err := command.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Input.Strategy != InputSteer {
		t.Fatalf("normalized strategy = %q, want %q", normalized.Input.Strategy, InputSteer)
	}
	if input.Strategy != "" {
		t.Fatalf("Normalize mutated caller strategy to %q", input.Strategy)
	}
}

func TestNormalizeSubmitInputTextLimit(t *testing.T) {
	base := func(text string) Command {
		return NewSubmitInput("command", "session", "input", text, InputSteer)
	}
	if _, err := base(strings.Repeat("x", MaxSubmitInputTextBytes)).Normalize(); err != nil {
		t.Fatalf("Normalize rejected text at limit: %v", err)
	}
	if _, err := base(strings.Repeat("x", MaxSubmitInputTextBytes+1)).Normalize(); err == nil {
		t.Fatal("Normalize accepted text above MaxSubmitInputTextBytes")
	}
}

func TestNormalizeCopiesPolicyPayloadsAndPreservesExplicitEmpty(t *testing.T) {
	allow := []string{"Read:*"}
	deny := []string{}
	command := Command{ID: "policy", SessionID: "session", Type: CommandSetPermissionPolicy,
		PermissionPolicy: &SetPermissionPolicy{Policy: PermissionPolicy{Mode: "default", AlwaysAllow: allow, AlwaysDeny: deny}}}
	normalized, err := command.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	allow[0] = "Bash:*"
	if normalized.PermissionPolicy.Policy.AlwaysAllow[0] != "Read:*" {
		t.Fatalf("normalized allow aliases caller: %v", normalized.PermissionPolicy.Policy.AlwaysAllow)
	}
	if normalized.PermissionPolicy.Policy.AlwaysDeny == nil {
		t.Fatal("explicit empty deny list became a mode-only nil list")
	}
}

func TestNormalizeExtensionCommandsAndBounds(t *testing.T) {
	command := Command{ID: "git", SessionID: "session", Type: CommandExternal,
		External: &ExternalCommand{Program: " GIT ", Args: []string{"status", "--short"}}}
	normalized, err := command.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.External.Program != "git" || normalized.External.Args[0] != "status" {
		t.Fatalf("external command was not normalized: %+v", normalized.External)
	}
	if _, err := (Command{ID: "bad", SessionID: "session", Type: CommandExternal,
		External: &ExternalCommand{Program: "sh"}}).Normalize(); err == nil {
		t.Fatal("arbitrary external executable was accepted")
	}
	if _, err := (Command{ID: "bad-apply", SessionID: "session", Type: CommandApply,
		Apply: &ApplyCommand{Path: ""}}).Normalize(); err == nil {
		t.Fatal("empty apply path was accepted")
	}
	checkpoint, err := (Command{ID: "checkpoint", SessionID: "session", Type: CommandCheckpoint,
		Checkpoint: &CheckpointCommand{}}).Normalize()
	if err != nil || checkpoint.Checkpoint.Action != CheckpointList {
		t.Fatalf("checkpoint default action = %+v, err=%v", checkpoint.Checkpoint, err)
	}
}

func TestNormalizeWorkflowCommands(t *testing.T) {
	tests := []struct {
		name    string
		command Command
		check   func(t *testing.T, command Command)
	}{
		{
			name: "review",
			command: Command{ID: "review", SessionID: "session", Type: CommandRunWorkflow,
				Workflow: &WorkflowCommand{Kind: WorkflowReview}},
			check: func(t *testing.T, command Command) {
				if command.Workflow == nil || command.Workflow.Kind != WorkflowReview || command.Workflow.Message != "" {
					t.Fatalf("normalized review workflow = %#v", command.Workflow)
				}
			},
		},
		{
			name: "commit",
			command: Command{ID: "commit", SessionID: "session", Type: CommandRunWorkflow,
				Workflow: &WorkflowCommand{Kind: WorkflowCommitPushPR, Message: "  ship it  "}},
			check: func(t *testing.T, command Command) {
				if command.Workflow == nil || command.Workflow.Kind != WorkflowCommitPushPR || command.Workflow.Message != "ship it" {
					t.Fatalf("normalized commit workflow = %#v", command.Workflow)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, err := test.command.Normalize()
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, command)
		})
	}
}

func TestNormalizeRejectsInvalidWorkflowCommands(t *testing.T) {
	cases := []Command{
		{ID: "unknown", SessionID: "session", Type: CommandRunWorkflow,
			Workflow: &WorkflowCommand{Kind: WorkflowKind("other")}},
		{ID: "review-message", SessionID: "session", Type: CommandRunWorkflow,
			Workflow: &WorkflowCommand{Kind: WorkflowReview, Message: "unexpected"}},
		{ID: "missing-message", SessionID: "session", Type: CommandRunWorkflow,
			Workflow: &WorkflowCommand{Kind: WorkflowCommitPushPR}},
		{ID: "nul-message", SessionID: "session", Type: CommandRunWorkflow,
			Workflow: &WorkflowCommand{Kind: WorkflowCommitPushPR, Message: "bad\x00message"}},
		{ID: "long-message", SessionID: "session", Type: CommandRunWorkflow,
			Workflow: &WorkflowCommand{Kind: WorkflowCommitPushPR, Message: strings.Repeat("x", maxCommandSummary+1)}},
	}
	for _, command := range cases {
		if _, err := command.Normalize(); err == nil {
			t.Errorf("Normalize(%s) accepted invalid workflow: %#v", command.ID, command.Workflow)
		}
	}
}

func TestNormalizeWorkflowRejectsOtherPayloads(t *testing.T) {
	command := Command{ID: "multiple", SessionID: "session", Type: CommandRunWorkflow,
		Workflow: &WorkflowCommand{Kind: WorkflowReview}, Init: &InitCommand{}}
	if _, err := command.Normalize(); err == nil {
		t.Fatal("workflow with another payload was accepted")
	}
}
