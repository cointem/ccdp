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
	"errors"
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
	switch s {
	case "manual":
		return ModeDefault, nil
	case "edits":
		return ModeAcceptEdits, nil
	case "bypass":
		return ModeBypass, nil
	}
	switch Mode(s) {
	case ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass:
		return Mode(s), nil
	}
	return "", fmt.Errorf("unknown permission mode %q (want manual|edits|bypass|plan)", s)
}

// Label returns the public name while persisted legacy values remain compatible.
func (m Mode) Label() string {
	switch m {
	case ModeDefault:
		return "manual"
	case ModeAcceptEdits:
		return "edits"
	case ModeBypass:
		return "bypass"
	}
	return string(m)
}

// Decision is the outcome of a permission check.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	DecisionAsk   Decision = "ask"
)

// ToolKind is assigned by the runtime from the resolved implementation, never
// from a model-controlled tool name. Unknown/custom implementations must ask.
type ToolKind uint8

const (
	KindUnknown ToolKind = iota
	KindRead
	KindState
	KindWrite
	KindShell
	KindProcess
	KindNetwork
	KindDelegate
)

func (k ToolKind) PlanAllowed() bool {
	switch k {
	case KindRead, KindState, KindNetwork, KindDelegate:
		return true
	default:
		return false
	}
}

func (k ToolKind) Concurrent() bool { return k == KindRead }

func (k ToolKind) NeedsReview() bool {
	return k == KindUnknown || k == KindWrite || k == KindShell || k == KindProcess || k == KindNetwork
}

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
// toolName is used to match user rules. kind comes from the runtime
// implementation classifier; shell arguments are inspected only for KindShell.
//
// Decision order is: persistent deny rules → session denies → persistent allow
// rules → session allows → per-tool classification. Persistent deny rules
// outrank even a session-remembered allow, so a "remember for this session"
// decision can never bypass always_deny.
func (m *Manager) Check(toolName string, args map[string]any, kind ToolKind) (Decision, string) {
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
	if mode == ModePlan && !kind.PlanAllowed() {
		return DecisionAsk, fmt.Sprintf("%s requires execute capability", toolName)
	}
	// Bash needs a command-aware gate: a whole-string glob rule can otherwise
	// approve a chained command (`git status && <anything>`) whose tail the user
	// never sanctioned. Handle Bash with the segment-aware path and return here;
	// every non-Bash tool falls through to the generic rule/session/bypass chain.
	if kind == KindShell {
		command := StringArg(args, "command", "")
		if commandAllowedByRules(command, policy.AlwaysAllow) {
			return DecisionAllow, "allowed by always_allow rule"
		}
		if allowedSession {
			return DecisionAllow, "allowed by session rule"
		}
		if allowAll || mode == ModeBypass {
			return DecisionAllow, "bypass mode"
		}
		return m.checkBash(mode, command)
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

	switch kind {
	case KindWrite:
		if mode == ModeAcceptEdits {
			return DecisionAllow, "edit accepted by acceptEdits mode"
		}
		return DecisionAsk, fmt.Sprintf("%s requires approval", toolName)
	case KindRead, KindState, KindNetwork, KindDelegate:
		return DecisionAllow, "built-in capability"
	default:
		return DecisionAsk, fmt.Sprintf("tool %q requires approval", toolName)
	}
}

// HardDeny reports a denial that no ordinary allow, bypass mode or approval
// hook may override. It is intentionally narrow so runtime can run it as the
// first admission check for custom tools and hooks.
func (m *Manager) HardDeny(toolName string, args map[string]any) (bool, string) {
	if m == nil {
		return true, "permission manager is nil"
	}
	invocations := []string{describeInvocation(toolName, args)}
	commands := []string{}
	if toolName == "Bash" {
		command := strings.TrimSpace(StringArg(args, "command", ""))
		commands = append(commands, command)
		// Static quoting/concatenation must not evade a deny, including the
		// quoted argv representation used by typed external commands. Analyze
		// individual segments so a denied tail cannot hide behind a safe head.
		// Dynamic shell programs remain outside this deliberately narrow parser;
		// this is not a claim to interpret arbitrary shell code.
		if segments, ok := analyzeShell(command); ok {
			for _, segment := range segments {
				commands = append(commands, segment.text, strings.Join(segment.words, " "))
			}
		}
		for _, command := range commands {
			invocations = append(invocations, "Bash:"+command)
		}
	}
	m.mu.RLock()
	policy := m.Policy
	deniedSession := m.denyMap[SessionKey(toolName, args)]
	for _, command := range commands {
		deniedSession = deniedSession || m.denyMap[CommandKey(command)]
	}
	m.mu.RUnlock()
	for _, deny := range policy.AlwaysDeny {
		for _, invocation := range invocations {
			if ruleMatches(deny, toolName, invocation) {
				return true, fmt.Sprintf("denied by always_deny rule %q", deny)
			}
		}
	}
	if deniedSession {
		return true, "denied by session rule"
	}
	if toolName == "Bash" {
		if reason := dangerousShellCommand(StringArg(args, "command", ""), 0); reason != "" {
			return true, "dangerous command pattern: " + reason
		}
	}
	return false, ""
}

// IsHardDenied is a compact bool-only helper for hooks and adapters.
func (m *Manager) IsHardDenied(toolName string, args map[string]any) bool {
	denied, _ := m.HardDeny(toolName, args)
	return denied
}

// ValidatePermissionRule reports whether a rule is worth storing. It refuses
// the over-broad "allow every Bash command" forms, which silently disable the
// command gate. kind is "allow" or "deny".
func ValidatePermissionRule(kind, rule string) error {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return errors.New("permission rule must not be empty")
	}
	if strings.ContainsRune(rule, '\n') {
		return errors.New("permission rule must be a single line")
	}
	if kind == "allow" {
		bare := strings.TrimSuffix(rule, ":*")
		bare = strings.TrimSuffix(bare, "*")
		bare = strings.TrimSuffix(bare, ":")
		if bare == "Bash" {
			return errors.New(`refusing to allow all of "Bash": that disables the command gate; allow a specific command such as Bash:git status* instead`)
		}
	}
	return nil
}

func (m *Manager) checkBash(mode Mode, command string) (Decision, string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return DecisionDeny, "empty command"
	}

	if mode == ModeBypass {
		return DecisionAllow, "bypass mode"
	}

	// Split into sequential command segments and require *every* one to be
	// individually read-only. A chain like `git status && rm -rf x` no longer
	// inherits the leading command's safety, and constructs we cannot tokenize
	// (command substitution, subshells, unbalanced quotes) fail closed to ask.
	segments, ok := analyzeShell(command)
	if !ok || len(segments) == 0 {
		return DecisionAsk, "command uses shell constructs that cannot be auto-approved"
	}
	for _, segment := range segments {
		if !readOnlyArgv(segment.words) {
			return DecisionAsk, "command is not in the safe allowlist"
		}
	}
	return DecisionAllow, "read-only command"
}

// commandAllowedByRules reports whether an always_allow rule set covers a Bash
// command. Unlike the historic whole-string glob, it splits the command into
// segments and requires every segment to be matched by some rule, so an allow
// rule for the head of a chain cannot authorize its tail. A bare `Bash` rule is
// an explicit whole-tool opt-in by the user and permits any command.
func commandAllowedByRules(command string, rules []string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	for _, rule := range rules {
		if rule == "Bash" {
			return true
		}
	}
	segments, ok := analyzeShell(command)
	if !ok || len(segments) == 0 {
		return false
	}
	for _, segment := range segments {
		covered := false
		for _, rule := range rules {
			if bashRuleCoversSegment(rule, segment.text) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// bashRuleCoversSegment matches one command segment against one allow rule.
// Rules may be written with or without the `Bash:` prefix; both forms glob the
// bare command text, anchored and case-insensitive.
func bashRuleCoversSegment(rule, segment string) bool {
	pattern := strings.TrimPrefix(rule, "Bash:")
	if pattern == "" {
		return false
	}
	ok, _ := globMatch(pattern, segment)
	return ok
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
	{regexp.MustCompile(`git\s+push\b[^\n]*(\s--force(-with-lease)?\b|\s-f\b)`), "force push"},
	{regexp.MustCompile(`git\s+reset\s+--hard`), "destructive git reset"},
	{regexp.MustCompile(`git\s+clean\s+-[a-zA-Z]*f`), "git clean with force"},
	{regexp.MustCompile(`git\s+(checkout|restore)\b[^\n]*\s(\.|\-\s|\-\-)\s*$`), "discard working tree changes"},
	{regexp.MustCompile(`git\s+checkout\s+--\s`), "discard working tree changes"},
}
