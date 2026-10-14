package tooling

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
)

// DecodeCalls is intentionally strict. UnmarshalArgs historically
// converted malformed, null, and array arguments into {}, which could turn a
// corrupt model response into a real default-argument side effect. Preserve
// JSON numbers with UseNumber so permission fingerprints and typed journal
// facts use the same value the provider returned.
func DecodeCalls(raw []llm.ToolCall) ([]messages.ToolCall, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	calls := make([]messages.ToolCall, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for i, call := range raw {
		if strings.TrimSpace(call.ID) == "" {
			return nil, fmt.Errorf("tool call %d has an empty id", i)
		}
		if seen[call.ID] {
			return nil, fmt.Errorf("tool call %q is duplicated", call.ID)
		}
		seen[call.ID] = true
		if strings.TrimSpace(call.Function.Name) == "" {
			return nil, fmt.Errorf("tool call %q has no tool name", call.ID)
		}
		rawArgs := strings.TrimSpace(call.Function.Arguments.String())
		if rawArgs == "" || rawArgs == "null" {
			return nil, fmt.Errorf("tool call %q has null or empty arguments", call.ID)
		}
		decoder := json.NewDecoder(strings.NewReader(rawArgs))
		decoder.UseNumber()
		var args map[string]any
		if err := decoder.Decode(&args); err != nil || args == nil {
			if err == nil {
				err = errors.New("arguments must be a JSON object")
			}
			return nil, fmt.Errorf("tool call %q arguments must be an object: %w", call.ID, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("tool call %q arguments contain trailing JSON", call.ID)
			}
			return nil, fmt.Errorf("tool call %q arguments are malformed: %w", call.ID, err)
		}
		calls = append(calls, messages.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: args})
	}
	return calls, nil
}
