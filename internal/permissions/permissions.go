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
	"sync"
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

// Manager answers permission questions for a session. It is safe for
// concurrent use: Check runs on tool goroutines while the TUI/agent loop
// mutates mode, policy and rememberance via the setters.
type Manager struct {
	mu       sync.RWMutex
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
func (m *Manager) SetMode(mode Mode) {
	m.mu.Lock()
	m.Mode = mode
	m.mu.Unlock()
}

// SetPolicy replaces the persistent rule set at runtime (settings reload).
func (m *Manager) SetPolicy(p Policy) {
	m.mu.Lock()
	m.Policy = p
	m.mu.Unlock()
}

// Mode returns the current mode.
func (m *Manager) CurrentMode() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Mode
}

// RememberAllow records a session-scoped "always allow" key.
func (m *Manager) RememberAllow(key string) {
	m.mu.Lock()
	m.allowMap[key] = true
	m.mu.Unlock()
}

// RememberDeny records a session-scoped "never allow" key.
func (m *Manager) RememberDeny(key string) {
	m.mu.Lock()
	m.denyMap[key] = true
	m.mu.Unlock()
}

// CommandKey builds the stable key used for command-level rememberance.
func CommandKey(command string) string { return "command:" + command }

// Check returns the decision for a tool invocation.
//
// toolName is e.g. "Bash", "Write". For Bash, args["command"] is inspected for
// classification. For file tools, the mode drives the decision.
//
// Decision order is: persistent deny rules → session denies → persistent allow
// rules → session allows → per-tool classification. Persistent deny rules
// outrank even a session-remembered allow, so a "remember for this session"
// decision can never bypass always_deny.
func (m *Manager) Check(toolName string, args map[string]any) (Decision, string) {
	m.mu.RLock()
	mode, allowAll := m.Mode, m.AllowAll
	policy := m.Policy
	_, deniedSession := m.denyMap[SessionKey(toolName, args)]
	_, allowedSession := m.allowMap[SessionKey(toolName, args)]
	m.mu.RUnlock()

	if allowAll || mode == ModeBypass {
		return DecisionAllow, "bypass mode"
	}

	// Rules are matched against the canonical invocation descriptor
	// ("Bash:<cmd>", "Write:<path>"), not the session-rememberance key form.
	invocation := describeInvocation(toolName, args)
	for _, deny := range policy.AlwaysDeny {
		if ruleMatches(deny, toolName, invocation) {
			return DecisionDeny, fmt.Sprintf("denied by always_deny rule %q", deny)
		}
	}
	if deniedSession {
		return DecisionDeny, "denied by session rule"
	}
	for _, allow := range policy.AlwaysAllow {
		if ruleMatches(allow, toolName, invocation) {
			return DecisionAllow, fmt.Sprintf("allowed by always_allow rule %q", allow)
		}
	}
	if allowedSession {
		return DecisionAllow, "allowed by session rule"
	}

	// Tool-specific classification.
	switch toolName {
	case "Bash":
		if mode == ModePlan {
			return DecisionAsk, "Bash is not available in plan mode"
		}
		return m.checkBash(mode, StringArg(args, "command", ""))
	case "Write", "Edit":
		if mode == ModePlan {
			return DecisionAsk, fmt.Sprintf("%s is not available in plan mode", toolName)
		}
		switch mode {
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
		if mode == ModeBypass {
			return DecisionAllow, "bypass mode"
		}
		return DecisionAsk, "GitCommit requires approval (mutates repository history)"
	case "WebFetch", "WebSearch":
		// Network access is a side effect; ask outside bypass mode.
		if mode == ModeBypass {
			return DecisionAllow, "bypass mode"
		}
		return DecisionAsk, fmt.Sprintf("%s requires approval (network access)", toolName)
	}

	// Unknown tool: ask to be safe.
	return DecisionAsk, fmt.Sprintf("unknown tool %q", toolName)
}

func (m *Manager) checkBash(mode Mode, command string) (Decision, string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return DecisionDeny, "empty command"
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
	if !hasDangerousOperator(command) && gitSafe(command) {
		return DecisionAllow, "safe git command"
	}

	// Everything else asks, even in acceptEdits (which only auto-accepts file edits).
	if mode == ModeBypass {
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

// globMatch does a simple * glob match. Patterns containing "/" (file-path
// rules) use path-glob semantics; bare patterns keep loose command-glob
// semantics.
func globMatch(pattern, s string) (bool, error) {
	return regexp.MatchString(globToRegex(pattern), s)
}

// globToRegex converts a permission rule glob to an anchored regex.
//
// Patterns whose value contains a "/" are treated as file paths: "*" matches
// within a single path segment ("[^/]*"), "**" spans segments (".*") and every
// other character is literal — so "Write:/tmp/*.log" matches /tmp/a.log but
// not /tmp/sub/a.log. Patterns without a "/" keep the loose historical
// semantics ("*" crosses anything, "?" matches one char), which command rules
// like "Bash:git status*" and bare rules like "go test *" rely on to match
// across spaces. Matching stays case-insensitive either way.
func globToRegex(pattern string) string {
	pathGlob := strings.Contains(pattern, "/")
	runes := []rune(pattern)
	var sb strings.Builder
	sb.WriteString("(?i)^")
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '*':
			if pathGlob && i+1 < len(runes) && runes[i+1] == '*' {
				sb.WriteString(".*")
				i++
			} else if pathGlob {
				sb.WriteString("[^/]*")
			} else {
				sb.WriteString(".*")
			}
		case r == '?' && !pathGlob:
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
	// `sudo` is deliberately NOT stripped: a sudo command must fall through to
	// the ask gate, not inherit the safety of the wrapped command.
	for {
		if len(s) == 0 {
			return ""
		}
		for _, prefix := range []string{"cd ", "env "} {
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

// hasDangerousOperator flags shell operators (including embedded newlines and
// command substitutions) that make a command unsafe to auto-allow.
func hasDangerousOperator(s string) bool {
	for _, op := range []string{">", ">>", "|", ";", "&&", "||", "$(", "`", "\n"} {
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
