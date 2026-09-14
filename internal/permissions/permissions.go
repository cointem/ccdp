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

// Effect describes the observable capability required by a tool invocation.
// Unknown is intentionally not treated as read-only; custom and MCP tools
// must opt into a narrower capability through runtime registration.
type Effect string

const (
	EffectRead     Effect = "read"
	EffectWrite    Effect = "write"
	EffectNetwork  Effect = "network"
	EffectProcess  Effect = "process"
	EffectDelegate Effect = "delegate"
	EffectPlan     Effect = "plan"
	EffectUnknown  Effect = "unknown"
)

// ExecutionMode is the workflow dimension. It is separate from Mode (the
// permission/approval policy) so plan cannot be accidentally implemented as a
// second mutable permission mode.
type ExecutionMode string

const (
	ExecutionModeExecute ExecutionMode = "execute"
	ExecutionModePlan    ExecutionMode = "plan"
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

// Snapshot is an immutable copy of a session permission manager.  Child
// runtimes use it at construction time so remembered approvals/denials are
// retained without sharing the parent's mutable approval maps or lock.
// Callers must treat the returned slices/maps as private to the snapshot.
type Snapshot struct {
	Mode     Mode
	Policy   Policy
	AllowAll bool
	AllowMap map[string]bool
	DenyMap  map[string]bool
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

// Snapshot returns a deep, credential-free copy of the current policy and
// session decisions.  It deliberately captures the remembered decisions as
// well as the configured rules: a child may inherit a decision already made,
// but it must never be able to mutate the parent's manager.
func (m *Manager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := Snapshot{
		Mode:     m.Mode,
		Policy:   Policy{AlwaysAllow: append([]string(nil), m.Policy.AlwaysAllow...), AlwaysDeny: append([]string(nil), m.Policy.AlwaysDeny...)},
		AllowAll: m.AllowAll,
		AllowMap: make(map[string]bool, len(m.allowMap)),
		DenyMap:  make(map[string]bool, len(m.denyMap)),
	}
	for k, v := range m.allowMap {
		s.AllowMap[k] = v
	}
	for k, v := range m.denyMap {
		s.DenyMap[k] = v
	}
	return s
}

// NewManagerFromSnapshot constructs an independent manager from a frozen
// snapshot.  The resulting manager can be safely changed by a child runtime
// without affecting the source session.
func NewManagerFromSnapshot(s Snapshot) *Manager {
	m := NewManager(s.Mode, s.Policy)
	m.AllowAll = s.AllowAll
	for k, v := range s.AllowMap {
		m.allowMap[k] = v
	}
	for k, v := range s.DenyMap {
		m.denyMap[k] = v
	}
	return m
}

// Clone returns an independent copy of the manager, including its
// session-scoped remembered decisions.
func (m *Manager) Clone() *Manager {
	return NewManagerFromSnapshot(m.Snapshot())
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
	m.Policy = Policy{
		AlwaysAllow: append([]string(nil), p.AlwaysAllow...),
		AlwaysDeny:  append([]string(nil), p.AlwaysDeny...),
	}
	m.mu.Unlock()
}

// SetAllowAll updates bypass mode under the manager lock. Bypass skips normal
// approval prompts but never bypasses HardDeny or plan capability limits.
func (m *Manager) SetAllowAll(allow bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.AllowAll = allow
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
	if m == nil {
		return DecisionDeny, "permission manager is nil"
	}
	if denied, reason := m.HardDeny(toolName, args); denied {
		return DecisionDeny, reason
	}
	m.mu.RLock()
	mode, allowAll := m.Mode, m.AllowAll
	policy := m.Policy
	_, allowedSession := m.allowMap[SessionKey(toolName, args)]
	m.mu.RUnlock()

	// Rules are matched against the canonical invocation descriptor
	// ("Bash:<cmd>", "Write:<path>"), not the session-rememberance key form.
	invocation := describeInvocation(toolName, args)
	// Plan is an execution capability cap, not an approval setting. Evaluate it
	// before allow rules and bypass so an allow hook/rule cannot turn plan into
	// execute mode. Existing ModePlan behavior is retained for compatibility;
	// new runtimes should use ExecutionMode + PlanAllows.
	if mode == ModePlan {
		if ok, reason := PlanAllows(toolName, args); !ok {
			return DecisionAsk, reason
		}
	}
	for _, allow := range policy.AlwaysAllow {
		if ruleMatches(allow, toolName, invocation) {
			return DecisionAllow, fmt.Sprintf("allowed by always_allow rule %q", allow)
		}
	}
	if allowedSession {
		return DecisionAllow, "allowed by session rule"
	}
	if allowAll || mode == ModeBypass {
		return DecisionAllow, "bypass mode"
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
		"ToolSearch", "ReadSkill", "EnterPlanMode", "ExitPlanMode", "AskUserQuestion":
		return DecisionAllow, "read-only tool"
	case "GitCommit":
		// Mutates history; bypass mode alone skips the gate.
		if mode == ModeBypass {
			return DecisionAllow, "bypass mode"
		}
		return DecisionAsk, "GitCommit requires approval (mutates repository history)"
	case "Agent":
		switch StringArg(args, "action", "") {
		case "list", "read", "output", "wait":
			return DecisionAllow, "read-only child observation"
		default:
			if mode == ModePlan {
				return DecisionDeny, "child control is unavailable in plan mode"
			}
			return DecisionAllow, "control within an existing delegated session; child policy remains enforced"
		}
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

// HardDeny reports a denial that no ordinary allow, bypass mode or approval
// hook may override. It is intentionally narrow so runtime can run it as the
// first admission check for custom tools and hooks.
func (m *Manager) HardDeny(toolName string, args map[string]any) (bool, string) {
	if m == nil {
		return true, "permission manager is nil"
	}
	m.mu.RLock()
	policy := m.Policy
	_, deniedSession := m.denyMap[SessionKey(toolName, args)]
	m.mu.RUnlock()
	invocation := describeInvocation(toolName, args)
	for _, deny := range policy.AlwaysDeny {
		if ruleMatches(deny, toolName, invocation) {
			return true, fmt.Sprintf("denied by always_deny rule %q", deny)
		}
	}
	if deniedSession {
		return true, "denied by session rule"
	}
	if toolName == "Bash" {
		command := strings.TrimSpace(StringArg(args, "command", ""))
		for _, pat := range denyPatterns {
			if pat.re.MatchString(command) {
				return true, fmt.Sprintf("dangerous command pattern: %s", pat.desc)
			}
		}
	}
	return false, ""
}

// CheckDenial is the decision-shaped form of HardDeny for runtimes that use
// Decision values throughout their admission pipeline.
func (m *Manager) CheckDenial(toolName string, args map[string]any) (Decision, string) {
	if denied, reason := m.HardDeny(toolName, args); denied {
		return DecisionDeny, reason
	}
	return DecisionAllow, "no hard deny"
}

// IsHardDenied is a compact bool-only helper for hooks and adapters.
func (m *Manager) IsHardDenied(toolName string, args map[string]any) bool {
	denied, _ := m.HardDeny(toolName, args)
	return denied
}

// InvocationEffects returns the conservative effects of a built-in tool.
// Custom/MCP tools should supply EffectUnknown unless their registration has
// independently verified a narrower capability.
func InvocationEffects(toolName string, args map[string]any) []Effect {
	switch toolName {
	case "Agent":
		switch StringArg(args, "action", "") {
		case "list", "read", "output", "wait":
			return []Effect{EffectRead}
		default:
			return []Effect{EffectDelegate}
		}
	case "Read", "Glob", "Grep", "LS", "GitStatus", "GitDiff", "GitLog", "ToolSearch", "ReadSkill":
		return []Effect{EffectRead}
	case "TodoWrite", "EnterPlanMode", "ExitPlanMode", "AskUserQuestion":
		return []Effect{EffectPlan}
	case "Write", "Edit", "GitCommit":
		return []Effect{EffectWrite}
	case "Bash", "ProcessStart", "ProcessWrite", "ProcessOutput", "ProcessStop":
		return []Effect{EffectProcess}
	case "WebFetch", "WebSearch":
		return []Effect{EffectNetwork}
	case "Task":
		return []Effect{EffectDelegate}
	default:
		return []Effect{EffectUnknown}
	}
}

// PlanAllows enforces the default plan capability cap. It is deliberately
// independent of allow/deny rules: a runtime may apply a narrower cap, but it
// cannot widen this default for an unclassified/custom capability.
func PlanAllows(toolName string, args map[string]any) (bool, string) {
	effects := InvocationEffects(toolName, args)
	for _, effect := range effects {
		switch effect {
		case EffectRead, EffectPlan:
			continue
		case EffectNetwork:
			return false, fmt.Sprintf("%s is not available in plan mode without an explicit network workflow", toolName)
		case EffectWrite, EffectProcess, EffectDelegate, EffectUnknown:
			return false, fmt.Sprintf("%s requires execute capability and is not available in plan mode", toolName)
		}
	}
	return true, "read-only/plan capability"
}

// CheckExecution applies an explicit workflow mode while retaining this
// Manager's permission policy. Runtime should call HardDeny first (or rely on
// this method's first step), then use the returned decision for approval.
func (m *Manager) CheckExecution(mode ExecutionMode, toolName string, args map[string]any) (Decision, string) {
	if denied, reason := m.HardDeny(toolName, args); denied {
		return DecisionDeny, reason
	}
	if mode == ExecutionModePlan {
		if ok, reason := PlanAllows(toolName, args); !ok {
			return DecisionAsk, reason
		}
	}
	return m.Check(toolName, args)
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
	// find is syntactically a read command, but these primaries execute a
	// mutation or an arbitrary child command. They must not inherit the
	// read-only first-token allowlist. Keep this token check conservative and
	// exact so names such as "-delete-old" are not rejected accidentally.
	if firstToken(s) == "find" {
		for _, token := range strings.Fields(s) {
			token = strings.Trim(token, "\"'")
			switch token {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir",
				"-fls", "-fprint", "-fprint0", "-fprintf":
				return true
			}
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
