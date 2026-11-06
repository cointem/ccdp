package tooling

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func BoundedFactText(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	return messages.TruncateUTF8(value, maxBytes)
}

// ApplyOutputBudget is the single model projection boundary. Limits count
// actual output bytes, in call order, without changing execution outcomes.
func ApplyOutputBudget(calls []messages.ToolCall, results []Result, perResultLimit int, limit, used int64) ([]Result, session.Budget, error) {
	projected := append([]Result(nil), results...)
	budget := session.Budget{Used: used}
	if limit > 0 {
		budget.Limit = limit
	}
	for i := range projected {
		outputLimit := int64(math.MaxInt)
		if perResultLimit > 0 {
			outputLimit = int64(perResultLimit)
		}
		if limit > 0 {
			outputLimit = min(outputLimit, max(int64(0), limit-budget.Used))
		}
		out := projected[i].Output
		omit := func() {
			projected[i].Output = ""
			projected[i].Code = nil
			projected[i].DisplayTruncated = true
			budget.Truncated = true
		}
		// Replay prefixes failed results unless the text already contains it.
		// Budget the final model content, including those exact seven bytes.
		const errorPrefix = "Error: "
		status := projected[i].Status
		if status == "" {
			status = "success"
			if projected[i].IsError {
				status = "error"
			}
		}
		isError := status != "success" && status != "ok"
		hadPrefix := isError && projected[i].Code == nil && strings.HasPrefix(out, errorPrefix)
		if isError {
			if outputLimit < int64(len(errorPrefix)) {
				omit()
				continue
			}
			outputLimit -= int64(len(errorPrefix))
			if hadPrefix {
				out = strings.TrimPrefix(out, errorPrefix)
			}
		}
		if projected[i].Code != nil {
			code, encoded, shortened, err := projectCodeResult(*projected[i].Code, outputLimit)
			if err != nil {
				return nil, budget, fmt.Errorf("tool %s output budget: %w", calls[i].ID, err)
			}
			projected[i].Code = &code
			out = encoded
			if out == "" {
				omit()
				continue
			}
			budget.Truncated = budget.Truncated || shortened
			projected[i].DisplayTruncated = projected[i].DisplayTruncated || shortened
		} else if projected[i].Process != nil {
			p, encoded := protocol.FitProcessOutput(*projected[i].Process, int(outputLimit))
			if encoded == "" {
				omit()
				continue
			}
			projected[i].Process = &p
			out = encoded
			projected[i].DisplayTruncated = projected[i].DisplayTruncated || p.Truncated
			budget.Truncated = budget.Truncated || p.Truncated
		} else if int64(len(out)) > outputLimit {
			budget.Truncated = true
			projected[i].DisplayTruncated = true
			marker := "\n[incomplete: tool output budget exhausted; use ReadOutput for the saved output]"
			if projected[i].OutputID != "" {
				marker = "\n[incomplete: use ReadOutput with output_id=" + projected[i].OutputID + "]"
			}
			if outputLimit >= int64(len(marker)) {
				out = messages.TruncateUTF8(out, int(outputLimit)-len(marker)) + marker
			} else {
				out = messages.TruncateUTF8(out, int(outputLimit))
			}
		}
		if hadPrefix {
			out = errorPrefix + out
		} else if isError {
			budget.Used += int64(len(errorPrefix))
		}
		budget.Used += int64(len(out))
		projected[i].Output = out
	}
	return projected, budget, nil
}

// projectCodeResult keeps the host facts and whole emitted values. All fit
// checks use the same JSON encoding as the model output, including escaping.
func projectCodeResult(code protocol.CodeResult, limit int64) (protocol.CodeResult, string, bool, error) {
	encoded, err := json.Marshal(code)
	if err != nil {
		return code, "", false, err
	}
	if int64(len(encoded)) <= limit {
		return code, string(encoded), false, nil
	}
	outputs, note, failure := code.Outputs, code.Note, code.Error
	code.Outputs = []json.RawMessage{}
	code.Note = ""
	fits := func() bool {
		encoded, _ = json.Marshal(code)
		return int64(len(encoded)) <= limit
	}
	if !fits() && failure != "" {
		// Preserve a nonempty error even when its message/stack must shrink.
		code.Error = "..."
		if fits() {
			low, high := 0, len(failure)
			for low < high {
				mid := low + (high-low+1)/2
				code.Error = messages.TruncateUTF8(failure, mid) + "..."
				if fits() {
					low = mid
				} else {
					high = mid - 1
				}
			}
			code.Error = messages.TruncateUTF8(failure, low) + "..."
		}
	}
	if !fits() {
		return code, "", true, nil
	}
	notice := "Result shortened by model output budget; full output is in the transcript."
	if note != "" {
		code.Note = notice + "\n" + note
	} else {
		code.Note = notice
	}
	if !fits() {
		code.Note = "Result shortened by model output budget."
		if !fits() {
			code.Note = ""
		}
	}
	low, high := 0, len(outputs)
	for low < high {
		mid := low + (high-low+1)/2
		code.Outputs = outputs[:mid]
		if fits() {
			low = mid
		} else {
			high = mid - 1
		}
	}
	code.Outputs = outputs[:low]
	// Keep [] rather than null for an empty list, including legacy callers.
	if len(code.Outputs) == 0 {
		code.Outputs = []json.RawMessage{}
	}
	encoded, _ = json.Marshal(code)
	return code, string(encoded), true, nil
}
