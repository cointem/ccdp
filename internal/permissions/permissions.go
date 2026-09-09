// Package permissions implements the approval gate. It fuses two ideas:
//
//   - Codex's structured per-tool permission gates with decision trees
//     (safe allowlist, dangerous denylist, everything else asks).
//   - Claude Code's permission modes (default / acceptEdits / bypassPermissions)
//     and always-allow / always-deny rule files.
//
// The Manager is session-scoped: transient "always allow this" decisions from
// the user are remembered for the duration of the session, while the Policy
// comes from the persistent configuration.
package permissions

import (
	"fmt"
	"regexp"
	"strings"
)

// Mode mirrors Claude Code's permission modes (including plan).
type Mode string

const (
	ModeDefault     Mode = "default"
	ModeAcceptEdits Mode = "acceptEdits"
	ModePlan        Mode = "plan"
	ModeBypass      Mode = "bypassPermissions"
)

// ValidModes lists selectable modes.
var ValidModes = []Mode{ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass}

// ParseMode converts a string to a Mode.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass:
		return Mode(s), nil
	}
	return "", fmt.Errorf("unknown permission mode %q (want default|acceptEdits|plan|bypassPermissions)", s)
}

// Decision is the outcome of a permission check.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	DecisionAsk   Decision = "ask"
)

// Policy is the persistent, config-file-supplied rule set.
type Policy struct {
	AlwaysAllow []string `json:"always_allow"`
	AlwaysDeny  []string `json:"always_deny"`
}

// Manager answers permission questions for a session.
type Manager struct {
	Mode     Mode
	Policy   Policy
	AllowAll bool            // bypass mode
	allowMap map[string]bool // session "always allow" rememberances
	denyMap  map[string]bool // session "never allow" rememberances
}

// NewManager builds a permission manager.
func NewManager(mode Mode, policy Policy) *Manager {
	return &Manager{
		Mode:     mode,
		Policy:   policy,
		allowMap: map[string]bool{},
		denyMap:  map[string]bool{},
	}
}

// SetMode switches the permission mode at runtime.
func (m *Manager) SetMode(mode Mode) { m.Mode = mode }

// SetPolicy replaces the persistent rule set at runtime (settings reload).
func (m *Manager) SetPolicy(p Policy) { m.Policy = p }

// Mode returns the current mode.
func (m *Manager) CurrentMode() Mode { return m.Mode }

// RememberAllow records a session-scoped "always allow" key.
func (m *Manager) RememberAllow(key string) { m.allowMap[key] = true }

// RememberDeny records a session-scoped "never allow" key.
func (m *Manager) RememberDeny(key string) { m.denyMap[key] = true }

// CommandKey builds the stable key used for command-level rememberance.
func CommandKey(command string) string { return "command:" + command }

// Check returns the decision for a tool invocation.
//
// toolName is e.g. "Bash", "Write". For Bash, args["command"] is inspected for
// classification. For file tools, the mode drives the decision.
func (m *Manager) Check(toolName string, args map[string]any) (Decision, string) {
	if m.AllowAll || m.Mode == ModeBypass {
		return DecisionAllow, "bypass mode"
	}

	// Session-level rememberance first.
	desc := SessionKey(toolName, args)
	if m.denyMap[desc] {
		return DecisionDeny, "denied by session rule"
	}
	if m.allowMap[desc] {
		return DecisionAllow, "allowed by session rule"
	}

	// Persistent policy rules (tool-level or tool:value globs). Rules are
	// matched against the canonical invocation descriptor ("Bash:<cmd>",
	// "Write:<path>"), not the session-rememberance key form.
	invocation := describeInvocation(toolName, args)
	for _, deny := range m.Policy.AlwaysDeny {
		if ruleMatches(deny, toolName, invocation) {
			return DecisionDeny, fmt.Sprintf("denied by always_deny rule %q", deny)
		}
	}
	for _, allow := range m.Policy.AlwaysAllow {
		if ruleMatches(allow, toolName, invocation) {
			return DecisionAllow, fmt.Sprintf("allowed by always_allow rule %q", allow)
		}
	}

	// Tool-specific classification.
	switch toolName {
	case "Bash":
		if m.Mode == ModePlan {
			return DecisionAsk, "Bash is not available in plan mode"
		}
		return m.checkBash(StringArg(args, "command", ""))
	case "Write", "Edit":
		if m.Mode == ModePlan {
			return DecisionAsk, fmt.Sprintf("%s is not available in plan mode", toolName)
		}
		switch m.Mode {
		case ModeAcceptEdits:
			return DecisionAllow, "edit accepted by acceptEdits mode"
		case ModeDefault:
			return DecisionAsk, fmt.Sprintf("%s requires approval in default mode", toolName)
		default:
			return DecisionAllow, "allowed by mode"
		}
	case "Read", "Glob", "Grep", "LS", "TodoWrite",
		"GitStatus", "GitDiff", "GitLog",
		"ToolSearch", "ReadSkill", "EnterPlanMode", "ExitPlanMode":
		return DecisionAllow, "read-only tool"
	case "GitCommit":
		// Mutates history; bypass mode alone skips the gate.
		if m.Mode == ModeBypass {
			return DecisionAllow, "bypass mode"
		}
		return DecisionAsk, "GitCommit requires approval (mutates repository history)"
	case "WebFetch", "WebSearch":
		// Network access is a side effect; ask outside bypass mode.
		if m.Mode == ModeBypass {
			return DecisionAllow, "bypass mode"
		}
		return DecisionAsk, fmt.Sprintf("%s requires approval (network access)", toolName)
	}

	// Unknown tool: ask to be safe.
	return DecisionAsk, fmt.Sprintf("unknown tool %q", toolName)
}

func (m *Manager) checkBash(command string) (Decision, string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return DecisionDeny, "empty command"
	}

	// Session rememberance for the whole command line.
	key := CommandKey(command)
	if m.denyMap[key] {
		return DecisionDeny, "denied by session rule"
	}
	if m.allowMap[key] {
		return DecisionAllow, "allowed by session rule"
	}

	// Dangerous patterns are always denied, regardless of mode.
	for _, pat := range denyPatterns {
		if pat.re.MatchString(command) {
			return DecisionDeny, fmt.Sprintf("dangerous command pattern: %s", pat.desc)
		}
	}

	// Safe read-only prefixes are always allowed.
	first := firstToken(command)
	if safeTokens[first] && !hasDangerousOperator(command) {
		return DecisionAllow, "read-only command"
	}
	if gitSafe(command) {
		return DecisionAllow, "safe git command"
	}

	// Everything else asks, even in acceptEdits (which only auto-accepts file edits).
	if m.Mode == ModeBypass {
		return DecisionAllow, "bypass mode"
	}
	return DecisionAsk, "command is not in the safe allowlist"
}

// describeInvocation produces a stable key for rule matching and rememberance.
func describeInvocation(toolName string, args map[string]any) string {
	switch toolName {
	case "Bash":
		return "Bash:" + StringArg(args, "command", "")
	default:
		return toolName + ":" + StringArg(args, "file_path", "")
	}
}

// SessionKey returns the key used for session-scoped allow/deny rememberance.
// The agent must call RememberAllow/RememberDeny with this same key.
func SessionKey(toolName string, args map[string]any) string {
	if toolName == "Bash" {
		return CommandKey(StringArg(args, "command", ""))
	}
	return describeInvocation(toolName, args)
}

// ruleMatches supports rules like "Bash" (whole tool) or "Bash:git status*"
// (command glob). Glob rules match the invocation value — the command line for
// Bash, file_path for file tools — not the prefixed descriptor.
func ruleMatches(rule, toolName, desc string) bool {
	if rule == toolName {
		return true
	}
	prefix := toolName + ":"
	if strings.HasPrefix(rule, prefix) {
		value := strings.TrimPrefix(rule, prefix)
		ok, _ := globMatch(value, strings.TrimPrefix(desc, prefix))
		return ok
	}
	// Bare command rules like "rm -rf *" apply to the command line of Bash
	// invocations (desc carries a "Bash:" prefix that the rule omits).
	if toolName == "Bash" && strings.Contains(rule, " ") {
		ok, _ := globMatch(rule, strings.TrimPrefix(desc, "Bash:"))
		return ok
	}
	return false
}

// globMatch does a simple * glob match.
func globMatch(pattern, s string) (bool, error) {
	return regexp.MatchString(globToRegex(pattern), s)
}

func globToRegex(pattern string) string {
	var sb strings.Builder
	sb.WriteString("(?i)^")
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	return sb.String()
}

func StringArg(args map[string]any, key, fallback string) string {
	if v, ok := args[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	// Strip environment assignments and `cd` prefixes which don't change safety.
	for {
		if len(s) == 0 {
			return ""
		}
		// skip leading cd / env / sudo
		for _, prefix := range []string{"cd ", "env ", "sudo "} {
			if strings.HasPrefix(s, prefix) {
				s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
			}
		}
		if i := strings.IndexAny(s, " \t;|&"); i > 0 {
			return s[:i]
		}
		return s
	}
}

// hasDangerousOperator flags commands combining operators with non-readonly first tokens.
func hasDangerousOperator(s string) bool {
	for _, op := range []string{">", ">>", "|", ";", "&&", "||", "$(", "`"} {
		if strings.Contains(s, op) {
			return true
		}
	}
	return false
}

// gitSafe allows a conservative subset of git commands.
func gitSafe(command string) bool {
	for _, p := range []string{
		"git status", "git diff", "git log", "git branch", "git remote",
		"git config --list", "git stash list", "git show", "git blame",
		"git rev-parse", "git shortlog", "git tag", "git describe",
	} {
		if strings.HasPrefix(command, p) {
			return true
		}
	}
	return false
}

// safeTokens is the allowlist of read-only command names.
var safeTokens = map[string]bool{
	"ls": true, "pwd": true, "date": true, "echo": true, "cat": true,
	"head": true, "tail": true, "wc": true, "grep": true, "rg": true,
	"find": true, "which": true, "whoami": true, "env": true, "printenv": true,
	"du": true, "df": true, "uname": true, "uptime": true, "true": true,
	"false": true, "sleep": true, "less": true, "file": true, "stat": true,
	"touch": true, "base64": true, "hexdump": true, "xxd": true, "tree": true,
	"nl": true, "diff": true,
}

type denyPattern struct {
	re   *regexp.Regexp
	desc string
}

var denyPatterns = []denyPattern{
	{regexp.MustCompile(`rm\s+(-[a-zA-Z]*f[a-zA-Z]*\s+)*/\s*$`), "rm -rf /"},
	{regexp.MustCompile(`rm\s+-rf\s+(\$HOME|~)/*$`), "rm -rf $HOME"},
	{regexp.MustCompile(`mkfs`), "mkfs (filesystem formatting)"},
	{regexp.MustCompile(`dd\s+.*of=/dev/`), "dd to block device"},
	{regexp.MustCompile(`\b(shutdown|reboot|poweroff|halt)\b`), "system power control"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\}\s*;:`), "fork bomb"},
	{regexp.MustCompile(`fdisk|parted\s`), "partition editing"},
	{regexp.MustCompile(`>\s*/dev/(sd|hd)`), "write to raw device"},
	{regexp.MustCompile(`curl\s+.*\|.*sh\s*$`), "curl|sh remote execution"},
	{regexp.MustCompile(`wget\s+.*\|.*sh\s*$`), "wget|sh remote execution"},
	{regexp.MustCompile(`chmod\s+-R\s*7\s*/\s*$`), "chmod -R 777 /"},
	{regexp.MustCompile(`:\(\)`), "fork bomb"},
	{regexp.MustCompile(`git\s+push\s+--force`), "force push"},
	{regexp.MustCompile(`git\s+reset\s+--hard`), "destructive git reset"},
	{regexp.MustCompile(`git\s+checkout\s+--\s`), "discard working tree changes"},
}
