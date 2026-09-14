package protocol

import (
	"fmt"
	"strings"
)

// CommandExternal is the narrow external-command contract used by the
// user-facing /git and GitHub command adapters. It is deliberately argv based
// and does not expose a shell string or an arbitrary executable path.
type ExternalCommand struct {
	Program string   `json:"program"`
	Args    []string `json:"args,omitempty"`
}

type ApplyCommand struct {
	Path string `json:"path"`
}

type CheckpointAction string

const (
	CheckpointList    CheckpointAction = "list"
	CheckpointCreate  CheckpointAction = "create"
	CheckpointRestore CheckpointAction = "restore"
)

type CheckpointCommand struct {
	Action  CheckpointAction `json:"action,omitempty"`
	ID      string           `json:"id,omitempty"`
	Summary string           `json:"summary,omitempty"`
}

type ExportCommand struct {
	Path string `json:"path,omitempty"`
}

type InitCommand struct{}

// WorkflowKind identifies one of the small, runtime-owned workspace
// workflows. The set is intentionally closed: clients submit a semantic
// operation and the runtime owns its fixed, gated steps.
type WorkflowKind string

const (
	WorkflowReview       WorkflowKind = "review"
	WorkflowCommitPushPR WorkflowKind = "commit_push_pr"
)

// WorkflowCommand is the typed payload for CommandRunWorkflow. Message is
// required only by WorkflowCommitPushPR; WorkflowReview is read-only and
// accepts no message.
type WorkflowCommand struct {
	Kind    WorkflowKind `json:"kind"`
	Message string       `json:"message,omitempty"`
}

func (c WorkflowCommand) normalize() (WorkflowCommand, error) {
	switch c.Kind {
	case WorkflowReview:
		if strings.TrimSpace(c.Message) != "" {
			return WorkflowCommand{}, fmt.Errorf("protocol: review workflow accepts no message")
		}
		c.Message = ""
	case WorkflowCommitPushPR:
		if err := validateCommandText("workflow message", c.Message, maxCommandSummary, true); err != nil {
			return WorkflowCommand{}, err
		}
		c.Message = strings.TrimSpace(c.Message)
	default:
		return WorkflowCommand{}, fmt.Errorf("protocol: unknown workflow kind %q", c.Kind)
	}
	return c, nil
}

// QueryKind selects one bounded, read-only runtime report. Queries are
// deliberately typed so a client cannot smuggle an arbitrary command through
// the report path; the Agent remains authoritative for all contents.
type QueryKind string

const (
	QueryConfig      QueryKind = "config"
	QueryDoctor      QueryKind = "doctor"
	QueryPermissions QueryKind = "permissions"
	QueryMemory      QueryKind = "memory"
	QueryMCP         QueryKind = "mcp"
	QueryPlugins     QueryKind = "plugins"
	QuerySkills      QueryKind = "skills"
	QueryStatus      QueryKind = "status"
	QueryGitHub      QueryKind = "github"
)

type QueryCommand struct {
	Kind QueryKind `json:"kind"`
}

// QueryReport is the stable result returned by Agent.Query and echoed in the
// command operation's report event. Text is already bounded by the runtime;
// Revision lets clients discard a report that predates a newer session view.
type QueryReport struct {
	Kind     QueryKind `json:"kind"`
	Revision Revision  `json:"revision"`
	Text     string    `json:"text"`
}

type WorkspaceCommand struct {
	Path string `json:"path"`
}

type TrustProjectCommand struct {
	Revoke bool `json:"revoke,omitempty"`
}

type ReloadCommand struct{}
type ClearMemoryCommand struct{}
type SaveSessionCommand struct{}

const (
	maxExternalArgs      = 64
	maxExternalArgBytes  = 16 << 10
	maxExternalTotalSize = 128 << 10
	maxCommandPathBytes  = 4 << 10
	maxCommandSummary    = 4 << 10
)

func validateCommandText(field, value string, limit int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("protocol: %s is required", field)
	}
	if len(value) > limit {
		return fmt.Errorf("protocol: %s exceeds %d bytes", field, limit)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("protocol: %s contains NUL", field)
	}
	return nil
}

func (c ExternalCommand) normalize() (ExternalCommand, error) {
	c.Program = strings.ToLower(strings.TrimSpace(c.Program))
	if c.Program != "git" && c.Program != "gh" {
		return ExternalCommand{}, fmt.Errorf("protocol: external program %q is not allowed", c.Program)
	}
	if len(c.Args) > maxExternalArgs {
		return ExternalCommand{}, fmt.Errorf("protocol: external command has too many args")
	}
	total := len(c.Program)
	c.Args = append([]string(nil), c.Args...)
	for i, arg := range c.Args {
		if err := validateCommandText(fmt.Sprintf("external arg %d", i), arg, maxExternalArgBytes, false); err != nil {
			return ExternalCommand{}, err
		}
		total += len(arg)
	}
	if total > maxExternalTotalSize {
		return ExternalCommand{}, fmt.Errorf("protocol: external command exceeds %d bytes", maxExternalTotalSize)
	}
	return c, nil
}

func (c ApplyCommand) normalize() (ApplyCommand, error) {
	if err := validateCommandText("apply path", c.Path, maxCommandPathBytes, true); err != nil {
		return ApplyCommand{}, err
	}
	c.Path = strings.TrimSpace(c.Path)
	return c, nil
}

func (c CheckpointCommand) normalize() (CheckpointCommand, error) {
	if c.Action == "" {
		c.Action = CheckpointList
	}
	switch c.Action {
	case CheckpointList:
		if c.ID != "" || c.Summary != "" {
			return CheckpointCommand{}, fmt.Errorf("protocol: checkpoint list accepts no id or summary")
		}
	case CheckpointCreate:
		if c.ID != "" {
			return CheckpointCommand{}, fmt.Errorf("protocol: checkpoint create accepts no id")
		}
		if err := validateCommandText("checkpoint summary", c.Summary, maxCommandSummary, false); err != nil {
			return CheckpointCommand{}, err
		}
	case CheckpointRestore:
		if err := validateCommandText("checkpoint id", c.ID, maxCommandPathBytes, true); err != nil {
			return CheckpointCommand{}, err
		}
		if c.Summary != "" {
			return CheckpointCommand{}, fmt.Errorf("protocol: checkpoint restore accepts no summary")
		}
	default:
		return CheckpointCommand{}, fmt.Errorf("protocol: unknown checkpoint action %q", c.Action)
	}
	return c, nil
}

func (c ExportCommand) normalize() (ExportCommand, error) {
	if err := validateCommandText("export path", c.Path, maxCommandPathBytes, false); err != nil {
		return ExportCommand{}, err
	}
	c.Path = strings.TrimSpace(c.Path)
	return c, nil
}
