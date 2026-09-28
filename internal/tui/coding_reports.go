package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Keep structured artifacts machine-readable on disk while presenting review
// locations and explicit next actions in the terminal transcript.
func codingReport(text string) string {
	var v struct {
		ReviewSessionID string `json:"review_session_id"`
		DurationMs      int64  `json:"duration_ms"`
		ID              string `json:"id"`
		Status          string `json:"status"`
		Summary         string `json:"summary"`
		Message         string `json:"message"`
		PlanID          string `json:"plan_id"`
		Apply           string `json:"apply"`
		Snapshot        string `json:"snapshot_id"`
		Findings        []struct {
			Severity    string `json:"severity"`
			Title       string `json:"title"`
			Path        string `json:"path"`
			Line        int    `json:"line"`
			Explanation string `json:"explanation"`
			Evidence    string `json:"evidence"`
		} `json:"findings"`
		Reviewed []string `json:"reviewed"`
		Skipped  []string `json:"skipped"`
		Edits    []struct {
			Path     string `json:"path"`
			Conflict string `json:"conflict"`
		} `json:"edits"`
	}
	if json.Unmarshal([]byte(text), &v) != nil {
		return text
	}
	var b strings.Builder
	if v.ReviewSessionID != "" {
		return v.Message
	}
	if v.Snapshot != "" || v.ID != "" && v.Findings != nil {
		status := map[string]string{"complete": "审查完成", "partial": "部分完成", "failed": "审查失败", "cancelled": "审查已停止"}[v.Status]
		if status == "" {
			status = v.Status
		}
		fmt.Fprintf(&b, "Review · %s", status)
		if v.DurationMs > 0 {
			fmt.Fprintf(&b, " · 用时 %s", formatElapsed(time.Duration(v.DurationMs)*time.Millisecond))
		}
		fmt.Fprintf(&b, "\n%s\nReport: %s\n", v.Summary, v.ID)
		if len(v.Findings) == 0 && v.Status == "complete" {
			b.WriteString("\n未发现明确缺陷。\n")
		}
		for _, f := range v.Findings {
			fmt.Fprintf(&b, "\n[%s] %s:%d — %s\n%s\nEvidence: %s\n", f.Severity, f.Path, f.Line, f.Title, f.Explanation, f.Evidence)
		}
		if v.Snapshot == "" {
			b.WriteString("\n快照未完成，本次审查尚未执行。")
		} else {
			fmt.Fprintf(&b, "\nReviewed %d files; skipped %d. Snapshot: %s", len(v.Reviewed), len(v.Skipped), v.Snapshot)
		}
		for _, p := range v.Skipped {
			fmt.Fprintf(&b, "\nSkipped: %s", p)
		}
		return b.String()
	}
	if v.PlanID != "" && len(v.Edits) > 0 {
		fmt.Fprintf(&b, "Plan: %s\n", v.PlanID)
		for _, edit := range v.Edits {
			if edit.Conflict != "" {
				fmt.Fprintf(&b, "  conflict: %s — %s\n", edit.Path, edit.Conflict)
			} else {
				fmt.Fprintf(&b, "  %s\n", edit.Path)
			}
		}
		fmt.Fprintf(&b, "\n%s", v.Apply)
		return b.String()
	}
	return text
}
