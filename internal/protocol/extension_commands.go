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
	WorkflowRestore      WorkflowKind = "restore"
	WorkflowMerge        WorkflowKind = "merge_child"
	WorkflowIndex        WorkflowKind = "index"
)

// WorkflowCommand is the typed payload for CommandRunWorkflow. Message is
// required when preparing WorkflowCommitPushPR; WorkflowReview accepts an
// optional message with additional review instructions.
type WorkflowCommand struct {
	Parent      int               `json:"parent,omitempty"`
	Kind        WorkflowKind      `json:"kind"`
	Message     string            `json:"message,omitempty"`
	Action      string            `json:"action,omitempty"`
	Scope       string            `json:"scope,omitempty"`
	Base        string            `json:"base,omitempty"`
	Commit      string            `json:"commit,omitempty"`
	ID          string            `json:"id,omitempty"`
	Mode        string            `json:"mode,omitempty"`
	Paths       []string          `json:"paths,omitempty"`
	Hunks       []string          `json:"hunks,omitempty"`
	Remote      string            `json:"remote,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	Title       string            `json:"title,omitempty"`
	Body        string            `json:"body,omitempty"`
	Draft       bool              `json:"draft,omitempty"`
	Resolutions map[string]string `json:"resolutions,omitempty"`
}

func (c WorkflowCommand) normalize() (WorkflowCommand, error) {
	switch c.Kind {
	case WorkflowReview:
		if err := validateCommandText("review message", c.Message, maxCommandSummary, false); err != nil {
			return WorkflowCommand{}, err
		}
		c.Message = strings.TrimSpace(c.Message)
		if c.Action != "" && c.Action != "list" && c.Action != "report" && c.Action != "save" {
			return WorkflowCommand{}, fmt.Errorf("unknown review action")
		}
		if (c.Action == "report" || c.Action == "save") && c.ID == "" {
			return WorkflowCommand{}, fmt.Errorf("review report requires id")
		}
		if (c.ID != "" && c.Action != "report" && c.Action != "save") || c.Mode != "" || len(c.Hunks) > 0 || len(c.Resolutions) > 0 {
			return WorkflowCommand{}, fmt.Errorf("review accepts scope, refs and paths only")
		}
		if c.Scope == "" {
			c.Scope = "uncommitted"
		}
		switch c.Scope {
		case "uncommitted", "staged", "branch", "commit":
		default:
			return WorkflowCommand{}, fmt.Errorf("unknown review scope %q", c.Scope)
		}
		if (c.Base != "" && c.Scope != "branch") || (c.Commit != "" && c.Scope != "commit") || (c.Parent != 0 && c.Scope != "commit") {
			return WorkflowCommand{}, fmt.Errorf("review refs do not match selected scope")
		}
		if c.Scope == "branch" && c.Base == "" {
			return WorkflowCommand{}, fmt.Errorf("branch review requires base")
		}
		if c.Scope == "commit" && c.Commit == "" {
			return WorkflowCommand{}, fmt.Errorf("commit review requires commit")
		}
	case WorkflowCommitPushPR:
		if c.Scope != "" && c.Scope != "all" && c.Scope != "staged" {
			return WorkflowCommand{}, fmt.Errorf("delivery scope must be all or staged")
		}
		if c.Action == "" {
			c.Action = "prepare"
		}
		switch c.Action {
		case "prepare", "apply", "resume", "inspect":
		default:
			return WorkflowCommand{}, fmt.Errorf("unknown delivery action %q", c.Action)
		}
		if (c.Action == "apply" || c.Action == "resume") && c.ID == "" {
			return WorkflowCommand{}, fmt.Errorf("delivery requires plan id")
		}
		if err := validateCommandText("workflow message", c.Message, maxCommandSummary, c.Action == "prepare"); err != nil {
			return WorkflowCommand{}, err
		}
		c.Message = strings.TrimSpace(c.Message)
	case WorkflowRestore, WorkflowMerge:
		if c.Scope != "" && c.Scope != "agent" && c.Scope != "snapshot" {
			return WorkflowCommand{}, fmt.Errorf("restore scope must be agent or snapshot")
		}
		if c.Kind == WorkflowMerge && c.Mode != "" && c.Mode != "code" {
			return WorkflowCommand{}, fmt.Errorf("merge supports code mode only")
		}
		if c.Action == "" {
			c.Action = "list"
		}
		switch c.Action {
		case "list", "prepare", "apply", "recover":
		default:
			return WorkflowCommand{}, fmt.Errorf("unknown action %q", c.Action)
		}
		if c.Action != "list" && c.ID == "" {
			return WorkflowCommand{}, fmt.Errorf("%s requires id", c.Action)
		}
		if c.Mode == "" {
			c.Mode = "code"
		}
		switch c.Mode {
		case "code", "conversation", "both":
		default:
			return WorkflowCommand{}, fmt.Errorf("unknown restore mode %q", c.Mode)
		}
	case WorkflowIndex:
		if c.Action == "" {
			c.Action = "status"
		}
		if c.Action != "status" && c.Action != "refresh" {
			return WorkflowCommand{}, fmt.Errorf("index action must be status or refresh")
		}
	default:
		return WorkflowCommand{}, fmt.Errorf("protocol: unknown workflow kind %q", c.Kind)
	}
	for _, v := range []string{c.Action, c.Scope, c.Base, c.Commit, c.ID, c.Mode} {
		if err := validateCommandText("workflow argument", v, maxCommandPathBytes, false); err != nil {
			return WorkflowCommand{}, err
		}
	}
	for _, v := range []string{c.Remote, c.Branch, c.Title, c.Body} {
		if err := validateCommandText("delivery argument", v, maxCommandPathBytes, false); err != nil {
			return WorkflowCommand{}, err
		}
	}
	if strings.HasPrefix(c.Remote, "-") || strings.HasPrefix(c.Branch, "-") {
		return WorkflowCommand{}, fmt.Errorf("invalid remote or branch")
	}
	if len(c.Resolutions) > 0 {
		if c.Action != "prepare" || (c.Kind != WorkflowRestore && c.Kind != WorkflowMerge) {
			return WorkflowCommand{}, fmt.Errorf("resolutions require restore or merge preparation")
		}
		choices := map[string]string{}
		for path, value := range c.Resolutions {
			if err := validateCommandText("resolution path", path, maxCommandPathBytes, true); err != nil {
				return WorkflowCommand{}, err
			}
			if value != "current" && value != "target" {
				return WorkflowCommand{}, fmt.Errorf("resolution must be current or target")
			}
			choices[path] = value
		}
		c.Resolutions = choices
	}
	if len(c.Paths) > 4096 || len(c.Hunks) > 4096 || len(c.Resolutions) > 4096 {
		return WorkflowCommand{}, fmt.Errorf("too many selected changes")
	}
	c.Hunks = append([]string(nil), c.Hunks...)
	c.Paths = append([]string(nil), c.Paths...)
	for _, v := range c.Paths {
		if err := validateCommandText("selected path", v, maxCommandPathBytes, true); err != nil {
			return WorkflowCommand{}, err
		}
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
