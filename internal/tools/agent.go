package tools

import (
	"ccdp/internal/protocol"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AgentTool is a narrow adapter; execution, delivery and reading live in the runtime.
type AgentTool struct{ name string }

func NewAgentTool(name string) *AgentTool { return &AgentTool{name: name} }
func (t *AgentTool) Name() string         { return t.name }
func (t *AgentTool) Description() string {
	switch t.name {
	case "SpawnAgent":
		return "Start an independent task. Returns a stable agent_id immediately; final results arrive automatically as collaboration messages. Defaults: worker/shared/fresh. Children cannot delegate."
	case "SendMessage":
		return "Send information to an agent (or parent) without starting another task. Not user authorization."
	case "FollowupAgent":
		return "Assign the next task to an idle agent. Busy agents reject follow-ups; use SendMessage for information during a task. Uses the same stable agent_id."
	case "StopAgent":
		return "Request cancellation of the agent's current task. Retains its session and saved results."
	case "WaitAgent":
		return "Wait for a new collaboration event or user input when you have no independent work. Does not read reports or target a particular run. No periodic timeout. Returns immediately if no work remains."
	case "ListAgents":
		return "List compact agent identities, live states and outcomes. No report bodies."
	default:
		return "Read saved child data without running a model. Defaults to the latest completed result (possibly the previous task if currently busy). Use view=transcript for history, or view=output and item_id for a full item. Continue using next_cursor."
	}
}
func (t *AgentTool) Parameters() map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	enum := func(v ...string) map[string]any { return map[string]any{"type": "string", "enum": v} }
	props := map[string]any{}
	required := []string{}
	switch t.name {
	case "SpawnAgent":
		props = map[string]any{"task": str("Self-contained assignment"), "name": str("Short display name"), "role": enum("worker", "explorer"), "workspace": enum("shared", "isolated"), "context": enum("fresh", "fork")}
		required = []string{"task"}
	case "SendMessage":
		props = map[string]any{"agent_id": str("Stable child agent_id, or parent"), "text": str("Information, not a new task")}
		required = []string{"agent_id", "text"}
	case "FollowupAgent", "StopAgent":
		props["agent_id"] = str("Stable agent_id")
		required = []string{"agent_id"}
		if t.name == "FollowupAgent" {
			props["task"] = str("Next assignment for an idle agent")
			required = append(required, "task")
		}
	case "ReadAgent":
		props = map[string]any{"agent_id": str("Stable agent_id"), "view": enum("result", "transcript", "output"), "cursor": str("next_cursor from the previous page"), "item_id": str("Required only for view=output")}
		required = []string{"agent_id"}
	case "ListAgents":
		props["cursor"] = str("next_cursor from the previous page")
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}
}
func (t *AgentTool) Run(ctx *Context) (string, error) {
	if err := ctx.checkResources(); err != nil {
		return "", err
	}
	// Tools may be called without provider-side schema validation.
	props := t.Parameters()["properties"].(map[string]any)
	for key := range ctx.Args {
		if _, ok := props[key]; !ok {
			return "", fmt.Errorf("%s: unknown parameter %q", t.name, key)
		}
	}
	encode := func(v any) (string, error) { b, err := json.Marshal(v); return string(b), err }
	if t.name == "WaitAgent" {
		if ctx.WaitAgentEvent == nil {
			return "", errors.New("collaboration wait unavailable")
		}
		reason, err := ctx.WaitAgentEvent(ctx.Context)
		if err != nil {
			return "", err
		}
		return encode(struct {
			Reason string `json:"reason"`
		}{reason})
	}
	if t.name == "SpawnAgent" {
		task := strings.TrimSpace(StringArg(ctx.Args, "task", ""))
		if task == "" || ctx.SpawnAgent == nil {
			return "", errors.New("task and child launcher required")
		}
		receipt, err := ctx.SpawnAgent(SpawnAgentRequest{Task: task, Name: StringArg(ctx.Args, "name", ""), Role: StringArg(ctx.Args, "role", "worker"), Workspace: StringArg(ctx.Args, "workspace", "shared"), Context: StringArg(ctx.Args, "context", "fresh")})
		if err != nil {
			return "", err
		}
		return encode(receipt)
	}
	id := protocol.SessionID(StringArg(ctx.Args, "agent_id", ""))
	if t.name == "SendMessage" {
		text := StringArg(ctx.Args, "text", "")
		if id == "" || strings.TrimSpace(text) == "" || ctx.SendAgentMessage == nil {
			return "", errors.New("agent_id, text and messaging capability required")
		}
		if err := ctx.SendAgentMessage(string(id), text); err != nil {
			return "", err
		}
		return encode(protocol.AgentReceipt{AgentID: id, Status: "queued"})
	}
	if ctx.Sessions == nil {
		return "", errors.New("agent directory unavailable")
	}
	if t.name == "ListAgents" {
		rows, err := ctx.Sessions.ListChildren(ctx.Context)
		if err != nil {
			return "", err
		}
		start := 0
		if cursor := StringArg(ctx.Args, "cursor", ""); cursor != "" {
			start, err = strconv.Atoi(cursor)
			if err != nil || start < 0 || start > len(rows) {
				return "", errors.New("invalid list cursor")
			}
		}
		page := protocol.AgentListPage{Agents: []protocol.AgentSummary{}}
		for i := start; i < len(rows); i++ {
			row := rows[i]
			name := row.Name
			if name == "" {
				name = row.Title
			}
			entry := protocol.AgentSummary{AgentReceipt: protocol.AgentReceiptFor(row.SessionID, row.Run), Name: truncateUTF8(name, 256)}
			page.Agents = append(page.Agents, entry)
			page.NextCursor = ""
			if i+1 < len(rows) {
				page.NextCursor = strconv.Itoa(i + 1)
			}
			b, _ := json.Marshal(page)
			if len(b) > ctx.outputLimit() {
				page.Agents = page.Agents[:len(page.Agents)-1]
				page.NextCursor = strconv.Itoa(i)
				if len(page.Agents) == 0 {
					return "", errors.New("output budget too small for an agent summary")
				}
				break
			}
		}
		return encode(page)
	}
	if id == "" {
		return "", errors.New("agent_id required")
	}
	switch t.name {
	case "ReadAgent":
		if ctx.ReadAgent == nil {
			return "", errors.New("agent reader unavailable")
		}
		return ctx.ReadAgent(ctx.Context, protocol.AgentReadRequest{AgentID: id, View: StringArg(ctx.Args, "view", "result"), Cursor: StringArg(ctx.Args, "cursor", ""), ItemID: StringArg(ctx.Args, "item_id", "")}, ctx.outputLimit())
	case "FollowupAgent", "StopAgent":
		if ctx.AgentCommandID == "" {
			return "", errors.New("command identity required")
		}
		control := protocol.AgentControl{ID: ctx.AgentCommandID, SessionID: id, Action: "cancel"}
		if t.name == "FollowupAgent" {
			control.Action = "continue"
			control.Text = StringArg(ctx.Args, "task", "")
			control.Strategy = protocol.InputFollowup
			if strings.TrimSpace(control.Text) == "" {
				return "", errors.New("task required")
			}
		}
		run, err := ctx.Sessions.Control(ctx.Context, control)
		if err != nil {
			return "", err
		}
		return encode(protocol.AgentReceiptFor(id, run))
	default:
		return "", fmt.Errorf("unsupported agent tool %q", t.name)
	}
}
