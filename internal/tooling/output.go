package tooling

import (
	"encoding/json"

	"ccdp/internal/messages"
	"ccdp/internal/session"
)

func BoundedFactText(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	return messages.TruncateUTF8(value, maxBytes)
}

// ApplyOutputBudget returns model-visible results in call order. Every call
// reserves its identity/status envelope before spending bytes on output, so
// parallel completion order cannot change which result gets truncated.
func ApplyOutputBudget(calls []messages.ToolCall, results []Result, limit, used int64) ([]Result, session.Budget) {
	projected := append([]Result(nil), results...)
	budget := session.Budget{Used: used}
	if limit > 0 {
		budget.Limit = limit
	}
	for i := range projected {
		out := projected[i].Output
		if limit > 0 {
			reserve := int64(32 + len(calls[i].ID))
			remaining := max(int64(0), limit-budget.Used)
			outputLimit := max(int64(0), remaining-reserve)
			if int64(len(out)) > outputLimit {
				budget.Truncated = true
				if json.Valid([]byte(out)) {
					out = ""
					projected[i].IsError = true
					projected[i].Status = "error"
				} else {
					marker := "\n[incomplete: tool output budget exhausted]"
					if outputLimit >= int64(len(marker)) {
						out = messages.TruncateUTF8(out, int(outputLimit)-len(marker)) + marker
					} else {
						out = ""
						projected[i].IsError = true
						projected[i].Status = "error"
					}
				}
			}
			budget.Used = min(limit, budget.Used+reserve+int64(len(out)))
		} else {
			budget.Used += int64(len(out))
		}
		projected[i].Output = out
	}
	return projected, budget
}
