package permissions

import "strings"

// CheckReadOnlyGit is exclusively for typed argv adapters that execute through
// execution.RunReadOnlyGit or StartReadOnlyGit's isolated query boundary.
// The trusted capability is an API boundary, never an argument-map flag that
// a model-supplied Bash call can forge. Raw Bash continues through Check.
func (m *Manager) CheckReadOnlyGit(argv []string) (Decision, string) {
	if m == nil {
		return DecisionDeny, "permission manager is nil"
	}
	frozen := m.Clone()
	// Honor both the human command-rule spelling and the quoted argv spelling
	// used for older remembered decisions and typed command approval prompts.
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	forms := []string{strings.Join(argv, " "), strings.Join(quoted, " ")}
	for _, command := range forms {
		if denied, reason := frozen.HardDeny("Bash", map[string]any{"command": command}); denied {
			return DecisionDeny, reason
		}
	}
	if !ReadOnlyGitArguments(argv) {
		return DecisionAsk, "Git arguments are not a verified read-only query"
	}
	for _, command := range forms {
		if decision, reason := frozen.Check("Bash", map[string]any{"command": command}, KindShell); decision == DecisionAllow {
			return decision, reason
		}
	}
	// These adapters have read capability even in plan mode: Git helpers and
	// pager/environment overrides are disabled at their execution boundary.
	return DecisionAllow, "read-only Git query with isolated execution"
}
