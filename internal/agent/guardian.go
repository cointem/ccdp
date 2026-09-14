package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"ccdp/internal/messages"
)

// GuardianSystem instructs the review sub-agent (Codex's guardian idea): a
// read-only pass that judges a pending tool call before it executes. The
// guardian must return strict JSON so the result is machine-parseable.
const GuardianSystem = `You are a security reviewer sub-agent. The main agent wants to execute a tool
call and you must decide whether it is safe enough to approve.

Operating rules:
- Use ONLY read-only tools (Read, Glob, Grep, LS, GitStatus, GitDiff, GitLog,
  ToolSearch) to inspect the context. Never modify files or run mutating
  commands.
- Approve only calls that are clearly safe and within the user's intent.
  Reject anything destructive, obviously malicious, or that bypasses controls.
- Reply with EXACTLY one line of JSON:
  {"approved": true|false, "reason": "short reason"}`

// guardianRisk reports whether a tool call warrants guardian review.
func guardianRisk(name string) bool {
	switch name {
	case "Bash", "Write", "Edit", "WebFetch", "WebSearch", "GitCommit":
		return true
	}
	return false
}

// guardianCheck runs a review sub-agent over a pending tool call. It returns
// nil when approved (or when the guardian budget is exhausted). Rejections
// come back as an error carrying the guardian's reason. Uses a circuit breaker
// of 3 reviews per turn.
func (a *Agent) guardianCheck(tc messages.ToolCall) error {
	a.mu.Lock()
	if a.guardianUses >= 3 {
		a.mu.Unlock()
		return nil // budget exhausted: proceed without review
	}
	a.guardianUses++
	a.mu.Unlock()

	a.emitStatus("guardian reviewing %s…", tc.Name)
	args, _ := json.Marshal(tc.Arguments)
	prompt := fmt.Sprintf("Approve or reject this tool call:\n\nTool: %s\nArguments: %s\n\nReturn the JSON verdict only.",
		tc.Name, string(args))

	// Guardian is a distinct child purpose. Do not route this through the Task
	// callback: the child constructor applies a strict read-only allowlist and
	// disables recursive guardian/Task execution.
	out, err := a.runGuardianChild(prompt)
	if err != nil {
		a.emitStatus("guardian review failed (%v) — proceeding", err)
		return nil
	}
	verdict, parsed := parseGuardianVerdict(out)
	if !parsed {
		// Fail-open is the documented trade-off (a hard block on unparseable
		// output would be too noisy), but it must be visible: this is exactly
		// the mode where the guard is absent.
		a.emitStatus("guardian review produced no valid verdict — allowing (fail-open)")
	}
	if verdict.Approved {
		a.emitStatus("guardian approved %s", tc.Name)
		return nil
	}
	reason := strings.TrimSpace(verdict.Reason)
	if reason == "" {
		reason = "rejected by guardian review"
	}
	a.emitStatus("guardian denied %s: %s", tc.Name, reason)
	return fmt.Errorf("blocked by guardian review: %s", reason)
}

type guardianVerdict struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason"`
}

// parseGuardianVerdict extracts the JSON verdict from the sub-agent output,
// tolerating surrounding prose. The bool reports whether a valid JSON verdict
// was found at all (callers surface fail-open).
func parseGuardianVerdict(out string) (guardianVerdict, bool) {
	var v guardianVerdict
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(out[start:end+1]), &v); err == nil {
			return v, true
		}
	}
	// Fallback: no JSON found — erring on the side of caution is too noisy;
	// treat unparseable output as approval with a note (the review already
	// cost a turn).
	return guardianVerdict{Approved: true}, false
}

// resetGuardianBudget clears the per-turn guardian circuit breaker.
func (a *Agent) resetGuardianBudget() {
	a.mu.Lock()
	a.guardianUses = 0
	a.mu.Unlock()
}
