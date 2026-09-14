package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/protocol"
)

type AgentTool struct{}

func NewAgentTool() *AgentTool  { return &AgentTool{} }
func (*AgentTool) Name() string { return "Agent" }
func (*AgentTool) Description() string {
	return "Inspect and control this root's child sessions. Task launches children; Agent lists progress, reads history/full output, waits, sends steer or followup input, interrupts, cancels, or explicitly continues a settled task. Mutations require the exact run_id from list to reject stale commands. No permission approvals or nested delegation are available. Read/wait never launch a model."
}
func (*AgentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"action"}, "properties": map[string]any{
			"action":     map[string]any{"type": "string", "enum": []string{"list", "read", "output", "wait", "send", "followup", "interrupt", "cancel", "continue"}},
			"session_id": map[string]any{"type": "string"}, "run_id": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"},
			"before": map[string]any{"type": "integer", "minimum": 0}, "item_id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 0},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60},
		}}
}
func (*AgentTool) Run(ctx *Context) (string, error) {
	if ctx.Sessions == nil {
		return "", errors.New("agent directory unavailable in this session")
	}
	var args struct {
		Action    string             `json:"action"`
		SessionID protocol.SessionID `json:"session_id"`
		RunID     protocol.RunID     `json:"run_id"`
		Text      string             `json:"text"`
		Before    uint64             `json:"before"`
		ItemID    string             `json:"item_id"`
		Offset    int64              `json:"offset"`
		Timeout   int                `json:"timeout_seconds"`
	}
	data, err := json.Marshal(ctx.Args)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &args); err != nil {
		return "", err
	}
	encode := func(value any, err error) (string, error) {
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(value)
		return string(data), err
	}
	if args.Action == "list" {
		return encode(ctx.Sessions.ListChildren(ctx.Context))
	}
	if args.SessionID == "" {
		return "", errors.New("session_id required")
	}
	switch args.Action {
	case "read":
		return encode(ctx.Sessions.ReadTranscript(ctx.Context, args.SessionID, args.Before, 64))
	case "output":
		return encode(ctx.Sessions.ReadOutput(ctx.Context, args.SessionID, args.ItemID, args.Offset, 32<<10))
	case "wait":
		timeout := args.Timeout
		if timeout <= 0 {
			timeout = 30
		}
		if timeout > 60 {
			timeout = 60
		}
		deadline := time.NewTimer(time.Duration(timeout) * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			rows, err := ctx.Sessions.ListChildren(ctx.Context)
			if err != nil {
				return "", err
			}
			var found *protocol.ChildSession
			for _, row := range rows {
				if row.SessionID == args.SessionID {
					copy := row
					found = &copy
					break
				}
			}
			if found == nil {
				return "", errors.New("session not found; use exact id from list")
			}
			if args.RunID != "" && found.Run.ID != args.RunID {
				return "", errors.New("stale run id")
			}
			if !found.Run.Active() || found.Approval != nil {
				return encode(found, nil)
			}
			select {
			case <-ctx.Context.Done():
				return "", ctx.Context.Err()
			case <-deadline.C:
				return encode(found, nil)
			case <-ticker.C:
			}
		}
	case "send", "followup", "interrupt", "cancel", "continue":
		if args.RunID == "" || ctx.AgentCommandID == "" {
			return "", errors.New("run_id and command identity required")
		}
		control := protocol.AgentControl{ID: ctx.AgentCommandID, SessionID: args.SessionID, RunID: args.RunID, Action: args.Action, Text: args.Text, Strategy: protocol.InputSteer}
		if args.Action == "followup" {
			control.Action = "send"
			control.Strategy = protocol.InputFollowup
		}
		return encode(ctx.Sessions.Control(ctx.Context, control))
	default:
		return "", fmt.Errorf("unsupported agent action %q", args.Action)
	}
}
