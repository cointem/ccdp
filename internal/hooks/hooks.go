// Package hooks implements a Claude-Code-style hooks system: shell commands
// that run at lifecycle points in the agent loop. Each hook receives a JSON
// payload on stdin describing the current context and can influence the
// loop:
//
//   - UserPromptSubmit: runs before a user message is sent to the model.
//     Returning {"decision":"block"} with a reason cancels the turn;
//     "additionalContext" is injected as a system message.
//   - PreToolUse: runs before a tool executes. Returning
//     {"decision":"block"|"deny"} vetoes the call, {"decision":"allow"}
//     short-circuits the permission gate, and an "ask" decision escalates to
//     the user.
//   - PostToolUse / PostToolUseFailure: run after a tool succeeds or fails.
//     Their JSON "hookSpecificOutput" is appended to the tool result.
//   - PreCompact / PostCompact: run around context compaction.
//   - Notification: a general signal (used e.g. when a long task finishes).
//   - Stop: runs when the user quits / the session ends.
//   - SessionStart / SessionEnd, SubagentStart / SubagentStop.
//
// Two configuration shapes are accepted per event, mirroring Claude Code:
//
//	"PreToolUse": ["cmd ..."]                                    // simple
//	"PreToolUse": [{"matcher":"Bash", "hooks":[{"command":"cmd", "timeout":30}]}]
//
// The matcher filters hooks by tool name (exact, "A|B" alternatives, or a
// regular expression); hooks without a matcher run for every invocation.
// A hook exiting with code 2 blocks (denies) the operation with its stderr as
// the reason; any other non-zero exit is a non-blocking error and the chain
// continues, matching Claude Code's semantics.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

// Event names, mirroring Claude Code's hook event identifiers.
const (
	EventUserPromptSubmit   = "UserPromptSubmit"
	EventPreToolUse         = "PreToolUse"
	EventPostToolUse        = "PostToolUse"
	EventPostToolUseFailure = "PostToolUseFailure"
	EventPreCompact         = "PreCompact"
	EventPostCompact        = "PostCompact"
	EventNotification       = "Notification"
	EventStop               = "Stop"
	EventSessionStart       = "SessionStart"
	EventSessionEnd         = "SessionEnd"
	EventSubagentStart      = "SubagentStart"
	EventSubagentStop       = "SubagentStop"
)

// ValidEvents lists all supported hook events.
var ValidEvents = []string{
	EventUserPromptSubmit, EventPreToolUse, EventPostToolUse,
	EventPostToolUseFailure, EventPreCompact, EventPostCompact,
	EventNotification, EventStop, EventSessionStart, EventSessionEnd,
	EventSubagentStart, EventSubagentStop,
}

// Input is the JSON payload delivered to every hook on stdin.
type Input struct {
	SessionID          string         `json:"session_id"`
	TranscriptPath     string         `json:"transcript_path,omitempty"`
	CWD                string         `json:"cwd"`
	PermissionMode     string         `json:"permission_mode"`
	HookEventName      string         `json:"hook_event_name"`
	ToolName           string         `json:"tool_name,omitempty"`
	ToolInput          map[string]any `json:"tool_input,omitempty"`
	ToolResponse       string         `json:"tool_response,omitempty"`
	Message            string         `json:"message,omitempty"`         // UserPromptSubmit text / notification text
	Input              string         `json:"input,omitempty"`           // alias for Message
	SubagentResult     string         `json:"subagent_result,omitempty"` // SubagentStop output
	StopHookActive     bool           `json:"stop_hook_active,omitempty"`
	HookSpecificOutput string         `json:"hookSpecificOutput,omitempty"`
}

// Decision is a hook verdict.
type Decision string

const (
	DecisionNone  Decision = ""
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	DecisionAsk   Decision = "ask"
	DecisionBlock Decision = "block" // UserPromptSubmit / exit code 2
)

// Output is what a hook may return via its JSON stdout.
type Output struct {
	Decision           Decision `json:"decision,omitempty"`
	Reason             string   `json:"reason,omitempty"`
	HookSpecificOutput string   `json:"hookSpecificOutput,omitempty"`
	AdditionalContext  string   `json:"additionalContext,omitempty"`
	Continue           *bool    `json:"continue,omitempty"`
}

// HookSpec is one hook command with an optional tool-name matcher and
// per-hook timeout (seconds; 0 = manager default).
type HookSpec struct {
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds
}

// Config maps an event name to its hooks.
type Config map[string][]HookSpec

// UnmarshalJSON accepts both the simple string form ("cmd") and the
// structured Claude form ({"matcher":…, "hooks":[{"command":…}]}).
func (c *Config) UnmarshalJSON(data []byte) error {
	// Simple form: map[string][]string.
	var simple map[string][]string
	if err := json.Unmarshal(data, &simple); err == nil {
		out := Config{}
		for ev, cmds := range simple {
			for _, cmd := range cmds {
				out[ev] = append(out[ev], HookSpec{Command: cmd})
			}
		}
		*c = out
		return nil
	}
	// Structured form: map[string][]group{matcher, hooks:[{command,timeout}]}.
	var structured map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &structured); err != nil {
		return fmt.Errorf("hooks: unsupported config shape (want [\"cmd\"] or [{matcher,hooks[]}]): %w", err)
	}
	out := Config{}
	for ev, groups := range structured {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out[ev] = append(out[ev], HookSpec{Matcher: g.Matcher, Command: h.Command, Timeout: h.Timeout})
			}
		}
	}
	*c = out
	return nil
}

// MarshalJSON writes back the simple form (keeps settings files readable).
func (c Config) MarshalJSON() ([]byte, error) {
	out := map[string][]string{}
	for ev, specs := range c {
		for _, s := range specs {
			out[ev] = append(out[ev], s.Command)
		}
	}
	return json.Marshal(out)
}

// Manager runs configured hooks for a session.
type Manager struct {
	mu   sync.RWMutex
	cfg  Config
	opts Options
	path string // transcript path for the current session
}

// Options carries session-level context into hooks.
type Options struct {
	SessionID string
	Workspace string
	Mode      string
	Timeout   time.Duration
	Env       []string // extra environment variables
	Sandbox   *sandbox.Sandbox
	// FailClosed turns execution, timeout, and malformed-output errors in
	// decision-affecting events into a deny. Observers keep historical
	// best-effort behavior when it is false.
	FailClosed bool
	// OutputLimit bounds one hook's stdout/stderr admission.
	OutputLimit int
}

// NewManager builds a hook manager from a config.
func NewManager(cfg Config, opts Options) *Manager {
	if cfg == nil {
		cfg = Config{}
	}
	if opts.Timeout <= 0 {
		// 60s default: enough for lints/tests-as-hooks, far below Claude's
		// 10min which lets a stuck hook freeze the whole turn.
		opts.Timeout = 60 * time.Second
	}
	if opts.OutputLimit <= 0 {
		opts.OutputLimit = 64 * 1024
	}
	return &Manager{cfg: cfg, opts: opts}
}

// Update replaces the hook configuration (e.g. after settings reload).
func (m *Manager) Update(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

// SetContext refreshes the mutable session values included in hook payloads.
// Agent state can change at runtime through /mode, /cd, /resume and /fork.
func (m *Manager) SetContext(sessionID, workspace, mode string) {
	m.mu.Lock()
	m.opts.SessionID = sessionID
	m.opts.Workspace = workspace
	m.opts.Mode = mode
	m.mu.Unlock()
}

// SetTranscript records the session transcript path exposed to hooks.
func (m *Manager) SetTranscript(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.path = path
}

// SetSandbox updates the execution boundary for subsequent hooks. Existing
// hook invocations own their context and cannot observe a policy swap midway.
func (m *Manager) SetSandbox(sb *sandbox.Sandbox) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.opts.Sandbox = sb
	m.mu.Unlock()
}

// Has reports whether any hook is configured for the event.
func (m *Manager) Has(event string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.cfg[event]) > 0
}

// List returns configured hook commands grouped by event (for /config UIs).
func (m *Manager) List() map[string][]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string][]string{}
	for ev, specs := range m.cfg {
		for _, s := range specs {
			label := s.Command
			if s.Matcher != "" {
				label = fmt.Sprintf("[%s] %s", s.Matcher, s.Command)
			}
			out[ev] = append(out[ev], label)
		}
	}
	return out
}

// baseInput builds the shared JSON payload.
func (m *Manager) baseInput(event string) Input {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return baseInputFrom(m.opts, m.path, event)
}

func baseInputFrom(opts Options, transcriptPath, event string) Input {
	return Input{
		SessionID:      opts.SessionID,
		TranscriptPath: transcriptPath,
		CWD:            opts.Workspace,
		PermissionMode: opts.Mode,
		HookEventName:  event,
	}
}

func cloneHookOptions(opts Options) Options {
	clone := opts
	clone.Env = append([]string(nil), opts.Env...)
	return clone
}

// matcherCache caches compiled matcher regexes (matchers are static config).
var matcherCache sync.Map // string -> *regexp.Regexp

// matcherMatches reports whether a hook spec's matcher admits the tool name.
// Empty matcher matches everything; "A|B" is an alternatives list; anything
// else is treated as a regular expression (Claude Code's semantics).
func matcherMatches(matcher, toolName string) bool {
	if matcher == "" || toolName == "" {
		return true
	}
	if strings.Contains(matcher, "|") && !strings.ContainsAny(matcher, "()[]{}*+?^$\\.") {
		for alt := range strings.SplitSeq(matcher, "|") {
			if strings.TrimSpace(alt) == toolName {
				return true
			}
		}
		return false
	}
	reAny, _ := matcherCache.LoadOrStore(matcher, compileOrnil(matcher))
	re, _ := reAny.(*regexp.Regexp)
	if re == nil {
		return matcher == toolName
	}
	return re.MatchString(toolName)
}

func compileOrnil(pattern string) any {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}

// Run executes every hook registered for event with the given mutation
// applied to the payload. Hooks whose matcher excludes the payload's tool
// name are skipped. The first vetoing decision is returned; additionalContext
// and hookSpecificOutput from all hooks are aggregated.
func (m *Manager) Run(ctx context.Context, event string, mutate func(*Input)) Output {
	// Callers outside a live turn (e.g. /compact before the first turn) may
	// pass a nil context; hooks must never panic on it.
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	specs := append([]HookSpec(nil), m.cfg[event]...)
	opts := cloneHookOptions(m.opts)
	transcriptPath := m.path
	m.mu.RUnlock()
	if len(specs) == 0 {
		return Output{}
	}

	in := baseInputFrom(opts, transcriptPath, event)
	if mutate != nil {
		mutate(&in)
	}

	var out Output
	for _, spec := range specs {
		if !matcherMatches(spec.Matcher, in.ToolName) {
			continue
		}
		hookTimeout := opts.Timeout
		if spec.Timeout > 0 {
			hookTimeout = time.Duration(spec.Timeout) * time.Second
		}
		res, blocked, err := m.runOne(ctx, spec.Command, hookTimeout, in, opts)
		if err != nil {
			if hookFailClosed(opts, event) {
				return Output{Decision: DecisionDeny, Reason: fmt.Sprintf("hook %q failed closed: %v", spec.Command, err)}
			}
			// Observer hooks retain best-effort behavior. Decision-affecting
			// hooks are fail-closed when the runtime opts into safety mode.
			if out.Reason == "" {
				out.Reason = fmt.Sprintf("hook %q failed: %v", spec.Command, err)
			}
			continue
		}
		if blocked {
			// Exit code 2: veto with stderr as the reason.
			return Output{Decision: DecisionBlock, Reason: res.Reason}
		}
		if res.Decision != DecisionNone {
			out.Decision = res.Decision
			out.Reason = res.Reason
			if res.Decision != DecisionAllow {
				return out // vetoing decision stops the chain
			}
		}
		if res.HookSpecificOutput != "" {
			out.HookSpecificOutput += res.HookSpecificOutput
		}
		if res.AdditionalContext != "" {
			out.AdditionalContext += res.AdditionalContext
		}
	}
	return out
}

// runOne executes a single hook command and parses its JSON output. The
// second return reports an exit code 2 (blocking) outcome.
func (m *Manager) runOne(ctx context.Context, command string, timeout time.Duration, in Input, opts Options) (Output, bool, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return Output{}, false, fmt.Errorf("hooks: marshal input: %w", err)
	}

	env := hookEnvironment(opts)
	res, err := execution.Run(execution.Request{
		Context: ctx, Command: command, Dir: opts.Workspace, Timeout: timeout,
		Input: bytes.NewReader(payload), Env: env, Sandbox: opts.Sandbox,
		OutputLimit: opts.OutputLimit,
		// Hook commands are a portable configuration contract, not an
		// interactive terminal session. Keep their POSIX shell semantics stable
		// across users whose login shell may be fish, nushell, or something else.
		Shell: "/bin/sh",
	})
	if err != nil {
		return Output{}, false, fmt.Errorf("hook %q: %w", command, err)
	}
	if res.TimedOut {
		return Output{}, false, fmt.Errorf("hook %q timed out", command)
	}
	if res.ExitCode != 0 {
		if res.ExitCode == 2 {
			reason := strings.TrimSpace(res.Stderr)
			if reason == "" {
				reason = "blocked by hook (exit code 2)"
			}
			return Output{Decision: DecisionDeny, Reason: reason}, true, nil
		}
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.ExitCode)
		}
		return Output{}, false, fmt.Errorf("hook %q: %s", command, msg)
	}

	out := Output{}
	trimmed := strings.TrimSpace(res.Stdout)
	if trimmed == "" {
		return out, false, nil
	}
	// A decision hook emitting non-JSON output is a failed contract. The
	// caller may still choose best-effort observer semantics, but fail-closed
	// runtimes will turn this into a deny rather than silently allowing.
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return Output{}, false, fmt.Errorf("hook %q returned invalid JSON: %w", command, err)
	}
	return out, false, nil
}

func hookFailClosed(opts Options, event string) bool {
	return opts.FailClosed && (event == EventPreToolUse || event == EventUserPromptSubmit || event == EventPreCompact)
}

func hookEnvironment(opts Options) []string {
	// Do not pass provider credentials or common secret-bearing values to a
	// project hook. The hook may receive explicit non-secret variables through
	// Options.Env, but the same filter is applied to those additions.
	entries := append(append([]string(nil), os.Environ()...), opts.Env...)
	out := execution.SanitizedEnvironmentFor(execution.EnvironmentCommand, entries)
	out = append(out, "CCDP_PROJECT_DIR="+opts.Workspace, "CCDP_SESSION_ID="+opts.SessionID)
	return out
}

// PreToolUse runs the PreToolUse hooks for a tool call and returns the verdict.
func (m *Manager) PreToolUse(ctx context.Context, toolName string, args map[string]any) Output {
	return m.Run(ctx, EventPreToolUse, func(in *Input) {
		in.ToolName = toolName
		in.ToolInput = args
	})
}

// PostToolUse runs the PostToolUse hooks for a finished tool call.
func (m *Manager) PostToolUse(ctx context.Context, toolName string, args map[string]any, result string) Output {
	return m.Run(ctx, EventPostToolUse, func(in *Input) {
		in.ToolName = toolName
		in.ToolInput = args
		in.ToolResponse = result
	})
}

// PostToolUseFailure runs the PostToolUseFailure hooks for a failed tool call.
func (m *Manager) PostToolUseFailure(ctx context.Context, toolName string, args map[string]any, errMsg string) Output {
	return m.Run(ctx, EventPostToolUseFailure, func(in *Input) {
		in.ToolName = toolName
		in.ToolInput = args
		in.ToolResponse = errMsg
	})
}

// UserPromptSubmit runs the UserPromptSubmit hooks for a new user message.
func (m *Manager) UserPromptSubmit(ctx context.Context, message string) Output {
	return m.Run(ctx, EventUserPromptSubmit, func(in *Input) {
		in.Message = message
		in.Input = message
	})
}

// PreCompact runs the PreCompact hooks.
func (m *Manager) PreCompact(ctx context.Context) Output {
	return m.Run(ctx, EventPreCompact, nil)
}

// PostCompact runs the PostCompact hooks.
func (m *Manager) PostCompact(ctx context.Context) Output {
	return m.Run(ctx, EventPostCompact, nil)
}

// Notify runs the Notification hooks.
func (m *Manager) Notify(ctx context.Context, text string) Output {
	return m.Run(ctx, EventNotification, func(in *Input) {
		in.Message = text
		in.Input = text
	})
}

// Stop runs the Stop hooks.
func (m *Manager) Stop(ctx context.Context) Output {
	return m.Run(ctx, EventStop, nil)
}

// SessionStart runs the SessionStart hooks.
func (m *Manager) SessionStart(ctx context.Context) Output {
	return m.Run(ctx, EventSessionStart, nil)
}

// SessionEnd runs the SessionEnd hooks (session is shutting down).
func (m *Manager) SessionEnd(ctx context.Context) Output {
	return m.Run(ctx, EventSessionEnd, nil)
}

// SubagentStart runs the SubagentStart hooks when a sub-agent launches.
func (m *Manager) SubagentStart(ctx context.Context, task string) Output {
	return m.Run(ctx, EventSubagentStart, func(in *Input) {
		in.Message = task
	})
}

// SubagentStop runs the SubagentStop hooks after a sub-agent finishes.
func (m *Manager) SubagentStop(ctx context.Context, result string) Output {
	return m.Run(ctx, EventSubagentStop, func(in *Input) {
		in.SubagentResult = result
	})
}
