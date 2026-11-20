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
		return "Delegate a task with the current project files and completed conversation history. Returns a stable agent_id. The final answer arrives automatically. Use workspace=isolated for a separate copy or context=fresh to omit history."
	case "SendMessage":
		return "Queue an update for your task child or parent without starting a turn. Final answers are already delivered automatically. A queued receipt confirms delivery to the mailbox. Messages do not grant permissions."
	case "FollowupAgent":
		return "Give an existing agent another assignment. A working agent receives it at a task boundary. An idle agent resumes its session."
	case "StopAgent":
		return "Request cancellation of the agent's current task. Retains its session and saved results."
	case "WaitAgent":
		return "Wait for a new collaboration event or user input when you have no independent work. Does not read reports or target a particular run. No periodic timeout. Returns immediately if no work remains."
	case "ListAgents":
		return "List compact agent identities, live states and outcomes. No report bodies."
	default:
		return "Look up a saved final answer by agent_id. For diagnostics, request view=transcript, then item_id for a full entry. Use next_cursor to continue a page."
	}
}
func (t *AgentTool) Parameters() map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	enum := func(v ...string) map[string]any { return map[string]any{"type": "string", "enum": v} }
	props := map[string]any{}
	required := []string{}
	switch t.name {
	case "SpawnAgent":
		props = map[string]any{"task": str("Task and expected deliverable"), "name": str("Short display name"), "role": enum("worker", "explorer"), "workspace": enum("shared", "isolated"), "context": enum("fresh", "fork")}
		props["role"].(map[string]any)["default"] = "worker"
		props["workspace"].(map[string]any)["default"] = "shared"
		props["context"].(map[string]any)["default"] = "fork"
		required = []string{"task"}
	case "SendMessage":
		props = map[string]any{"agent_id": str("Stable child agent_id, or parent"), "text": str("Information, not a new task")}
		required = []string{"agent_id", "text"}
	case "FollowupAgent", "StopAgent":
		props["agent_id"] = str("Stable agent_id")
		required = []string{"agent_id"}
		if t.name == "FollowupAgent" {
			props["task"] = str("Additional assignment")
			required = append(required, "task")
		}
	case "ReadAgent":
		props = map[string]any{"agent_id": str("Stable agent_id"), "view": enum("result", "transcript", "output"), "cursor": str("next_cursor from the previous page"), "item_id": str("Transcript entry to read in full")}
		required = []string{"agent_id"}
	case "ListAgents":
		props["cursor"] = str("next_cursor from the previous page")
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}
}
func (t *AgentTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	return executeText(ctx, args, func(ctx *Context) (string, error) {
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
			receipt, err := ctx.SpawnAgent(SpawnAgentRequest{Task: task, Name: StringArg(ctx.Args, "name", ""), Role: StringArg(ctx.Args, "role", "worker"), Workspace: StringArg(ctx.Args, "workspace", "shared"), Context: StringArg(ctx.Args, "context", "fork")})
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
			return ctx.ReadAgent(ctx.Context, protocol.AgentReadRequest{AgentID: id, View: StringArg(ctx.Args, "view", ""), Cursor: StringArg(ctx.Args, "cursor", ""), ItemID: StringArg(ctx.Args, "item_id", "")}, ctx.outputLimit())
		case "FollowupAgent", "StopAgent":
			if ctx.AgentCommandID == "" {
				return "", errors.New("command identity required")
			}
			control := protocol.AgentControl{ID: ctx.AgentCommandID, SessionID: id, Action: "cancel"}
			if t.name == "FollowupAgent" {
				control.Action = "followup"
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
	})
}
