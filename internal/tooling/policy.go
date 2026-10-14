package tooling

import (
	"strings"

	"ccdp/internal/hooks"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
)

// Authority is scoped to one frozen invocation. The session keeps hard
// admission, approval identity and durable start/completion; the executor owns
// policy ordering. An allow decision never bypasses another policy's denial.
type Authority interface {
	Admit() (Admission, *Result)
	GoPreHook() (plugin.ToolDecision, string)
	ShellPreHook() hooks.Output
	CheckPermission() (permissions.Decision, string)
	Approve(reason string) bool
	CheckReady() error
	Invoke() Result
	Rejected(status, reason string)
}
type Admission struct{ ConfiguredAllow, NeedsCapabilities bool }

func (e Executor) Run(a Authority) Result {
	admission, rejected := a.Admit()
	if rejected != nil {
		return *rejected
	}
	deny := func(output, reason string) Result {
		a.Rejected("denied", reason)
		return Result{Output: output, IsError: true, Status: "denied"}
	}
	hookAllow := false
	var asks []string
	decision, reason := a.GoPreHook()
	switch decision {
	case plugin.DecisionDeny:
		if reason == "" {
			reason = "blocked by Go pre-tool hook"
		}
		return deny("blocked by hook: "+reason, reason)
	case plugin.DecisionAsk:
		if reason == "" {
			reason = "Go hook requests approval"
		}
		asks = append(asks, reason)
	case plugin.DecisionAllow:
		hookAllow = true
	}
	output := a.ShellPreHook()
	if output.Continue != nil && !*output.Continue {
		reason := output.Reason
		if reason == "" {
			reason = "stopped by PreToolUse hook"
		}
		return deny("stopped by hook: "+reason, reason)
	}
	switch output.Decision {
	case hooks.DecisionDeny, hooks.DecisionBlock:
		reason := output.Reason
		if reason == "" {
			reason = "blocked by PreToolUse hook"
		}
		return deny("blocked by hook: "+reason, reason)
	case hooks.DecisionAsk:
		reason := output.Reason
		if reason == "" {
			reason = "PreToolUse hook requests approval"
		}
		asks = append(asks, reason)
	case hooks.DecisionAllow:
		hookAllow = true
	}
	permission, reason := a.CheckPermission()
	if permission == permissions.DecisionDeny {
		return deny("permission denied: "+reason, reason)
	}
	if admission.ConfiguredAllow || hookAllow {
		permission = permissions.DecisionAllow
	}
	if len(asks) > 0 {
		permission = permissions.DecisionAsk
		reason = strings.Join(asks, "; ")
	}
	if permission == permissions.DecisionAsk || admission.NeedsCapabilities {
		if permission != permissions.DecisionAsk {
			reason = "This tool needs additional sandbox access"
		}
		if !a.Approve(reason) {
			return deny("permission denied by user: "+reason, reason)
		}
	}
	if err := a.CheckReady(); err != nil {
		return Result{Output: err.Error(), IsError: true, Status: "error"}
	}
	return a.Invoke()
}
