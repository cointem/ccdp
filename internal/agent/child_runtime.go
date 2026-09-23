package agent

// This file contains the shared child-runtime contract.  The provider/runtime
// owner wires these values into Agent's single construction path; Task and the
// guardian only use the lifecycle runner in subagent.go.  Keeping the options
// small makes the frozen boundary explicit and avoids a second constructor
// which could accidentally reload project settings or providers.

import (
	"context"
	"errors"
	"fmt"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/plugin"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

// Options is the frozen input passed to the provider-owned child constructor.
// The config must already include all parent effective/project settings;
// child construction must not load settings again. ProviderRegistry and
// Permissions are snapshots/copies, never mutable parent handles. An
// AllowedTools nil map means the frozen registry may be used as-is, while a
// non-nil map (including empty) is an exact hard allowlist.
type Options struct {
	Supervisor            *SessionSupervisor
	RootContext           context.Context
	EffectiveConfigFrozen bool
	ProviderRegistry      *plugin.ModelRegistry
	Permissions           *permissions.Manager
	NonInteractive        bool
	AllowedTools          map[string]bool
	Purpose               string
	ParentSessionID       string
	toolBindingDigest     string
	memoryResume          []session.Record
	memoryBlobs           *memoryRequestArtifacts
}

const (
	childPurposeTask     = "task"
	childPurposeGuardian = "guardian"
	defaultChildWorkers  = 4
)

// guardianReadOnlyTools is an execution allowlist, not merely prompt text.
// The provider's executeTool path must invoke ChildToolGate before registry
// lookup, hooks, permission checks, or Tool.Run.
var guardianReadOnlyTools = map[string]bool{
	"Read":       true,
	"Glob":       true,
	"Grep":       true,
	"LS":         true,
	"GitStatus":  true,
	"GitDiff":    true,
	"GitLog":     true,
	"ToolSearch": true,
	"ReadSkill":  true,
}

// childRuntimeState is stored directly on Agent by the provider-owned
// constructor. It is intentionally not a package-level registry: lifecycle
// state must be released with the Agent and must not outlive a session.
type childRuntimeState struct {
	purpose        string
	nonInteractive bool
	allowed        map[string]bool
	parentSession  string
	perms          *permissions.Manager
}

// childStepSnapshot is populated by the provider/runtime owner at the
// beginStep boundary. It lets a Task/guardian launched from a tool use the
// exact effective config and model binding that produced that tool call,
// rather than a later live config/reload. The owner stores it on Agent; this
// type deliberately carries only frozen values and no parent pointer.
type childStepSnapshot struct {
	mcpLease      *mcp.StepLease
	cfg           config.Config
	binding       modelBinding
	models        *plugin.ModelRegistry
	registryNames []string
	// Tools are immutable. The supervisor retains this exact MCP generation
	// independently so notify children can outlive the parent step.
	toolLease *tools.Lease
	// preHooks is a frozen copy of trusted in-process pre-tool decisions. The
	// child receives callbacks, never the mutable parent registry.
	preHooks []plugin.PreToolHook
	turn     uint64
	revision uint64
}

// childSlots is stored on the parent Agent and shared by all Task calls in
// that parent turn/session. A bounded semaphore prevents concurrent Task calls
// from multiplying unbounded child goroutines.
type childSlots struct {
	sem chan struct{}
	max int
}

func newChildSlots(max int) *childSlots {
	if max <= 0 {
		max = defaultChildWorkers
	}
	if max > 16 {
		max = 16
	}
	return &childSlots{sem: make(chan struct{}, max), max: max}
}

func slotsForParent(a *Agent) *childSlots {
	if a == nil {
		return newChildSlots(1)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.childSlots == nil {
		limit := defaultChildWorkers
		if a.cfg != nil && a.cfg.MaxParallelTools > 0 {
			limit = a.cfg.MaxParallelTools
		}
		a.childSlots = newChildSlots(limit)
	}
	return a.childSlots
}

func (s *childSlots) acquire(ctx context.Context) error {
	if s == nil || s.sem == nil {
		return errors.New("child slots are unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *childSlots) release() {
	if s == nil || s.sem == nil {
		return
	}
	select {
	case <-s.sem:
	default:
	}
}

func cloneChildAllowed(src map[string]bool) map[string]bool {
	if src == nil {
		return nil
	}
	dst := make(map[string]bool, len(src))
	for name, enabled := range src {
		dst[name] = enabled
	}
	return dst
}

// childDecisionHooks is the only portion of a parent's shell-hook config that
// a Task is allowed to inherit.  Prompt/tool decisions remain useful for the
// child, while lifecycle hooks are intentionally omitted so constructing a
// child cannot replay SessionStart/SessionEnd or parent Task lifecycle work.
func childDecisionHooks(src hooks.Config) hooks.Config {
	if len(src) == 0 {
		return hooks.Config{}
	}
	allowed := map[string]bool{
		hooks.EventUserPromptSubmit: true,
		hooks.EventPreToolUse:       true,
	}
	dst := hooks.Config{}
	for event, specs := range src {
		if allowed[event] {
			dst[event] = append([]hooks.HookSpec(nil), specs...)
		}
	}
	return dst
}

// pinChildProvider returns an immutable child-owned registry whose route for
// the bound model resolves to the exact provider captured by beginStep. The
// provider pointer is part of modelBinding, while the registry is only the
// constructor's lookup surface; pinning both prevents a same-name route swap
// from changing the child between parent stream and child construction.
func pinChildProvider(source *plugin.ModelRegistry, cfg config.Config, binding modelBinding) *plugin.ModelRegistry {
	var pinned *plugin.ModelRegistry
	if source == nil {
		pinned = plugin.NewModelRegistry()
	} else {
		pinned = source.Clone()
	}
	if binding.client != nil && binding.provider != "" && binding.model != "" {
		pinned.Register(binding.client)
		if binding.routeKind == "http" {
			pinned.RouteDefault(binding.model, binding.provider,
				binding.httpBinding(cfg.ResolveProvider(binding.model).APIKey))
		} else {
			pinned.Route(binding.model, binding.provider)
		}
	}
	return pinned.Freeze()
}

// ChildToolGate is the non-overridable child admission check. Provider glue
// should call it before normal Agent.executeTool admission. For non-child
// Agents it returns an allow/no-op result. Approval requests in a
// non-interactive child are denials, never implicit approvals.
func ChildToolGate(a *Agent, tc messages.ToolCall) (bool, string) {
	if a == nil || a.childState == nil {
		return false, ""
	}
	s := a.childState
	if s.purpose == childPurposeGuardian && !guardianReadOnlyTools[tc.Name] {
		return true, fmt.Sprintf("guardian hard deny: tool %q is outside the read-only allowlist", tc.Name)
	}
	if s.allowed != nil && !s.allowed[tc.Name] {
		return true, fmt.Sprintf("child hard deny: tool %q is outside the frozen allowlist", tc.Name)
	}
	// Task is deliberately one level only for this batch. This remains a hard
	// gate even when a caller passed a nil allowlist.
	if s.purpose == childPurposeTask && (tc.Name == "Task" || tc.Name == "Agent") {
		return true, "child hard deny: nested Task is not available"
	}
	if s.purpose == childPurposeGuardian {
		for _, effect := range permissions.InvocationEffects(tc.Name, tc.Arguments) {
			if effect != permissions.EffectRead {
				return true, fmt.Sprintf("guardian hard deny: tool %q is not read-only", tc.Name)
			}
		}
	}
	if s.nonInteractive && s.perms != nil {
		decision, reason := s.perms.Check(tc.Name, tc.Arguments)
		if decision == permissions.DecisionAsk {
			if reason == "" {
				reason = "permission requires approval"
			}
			return true, "child non-interactive: " + reason
		}
	}
	return false, ""
}

// ChildRuntime returns a read-only copy of child metadata for provider glue
// and diagnostics. The returned allowlist is always independent.
func ChildRuntime(a *Agent) (purpose string, nonInteractive bool, allowed map[string]bool, ok bool) {
	if a == nil || a.childState == nil {
		return "", false, nil, false
	}
	s := a.childState
	return s.purpose, s.nonInteractive, cloneChildAllowed(s.allowed), true
}

func parentTurnContext(a *Agent) context.Context {
	if a == nil {
		return context.Background()
	}
	a.mu.Lock()
	ctx := a.turnCtx
	if ctx == nil {
		ctx = a.rootCtx
	}
	a.mu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// childToolLeaseFromParent returns the immutable tool view captured for the
// currently executing parent step.  A child may borrow MCP/plugin tools from
// this view, but it cannot retain the parent registry or manager: the parent
// step owns the lease and releases it after all tool calls (including the
// synchronous child join) have completed.
func childToolLeaseFromParent(a *Agent) *tools.Lease {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.busy || a.childStep == nil || a.childStep.turn != a.turnSeq {
		return nil
	}
	return a.childStep.toolLease
}

// childPreHooksFromParent snapshots only trusted in-process pre-tool
// decisions. A child receives its own GoHooks registry populated from this
// slice; no mutable parent registry or post/lifecycle observer is retained.
func childPreHooksFromParent(a *Agent) []plugin.PreToolHook {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy && a.childStep != nil && a.childStep.turn == a.turnSeq {
		return append([]plugin.PreToolHook(nil), a.childStep.preHooks...)
	}
	if a.gohooks == nil {
		return nil
	}
	return a.gohooks.SnapshotPreTool()
}

// inheritBorrowedPreHooks installs a frozen copy of parent pre-tool decisions
// in the child-owned GoHooks registry. ChildToolGate still runs before these
// callbacks, so an inherited callback can deny/ask but cannot relax guardian
// or non-interactive hard policy.
func (a *Agent) inheritBorrowedPreHooks(preHooks []plugin.PreToolHook) {
	if a == nil || a.childState == nil || len(preHooks) == 0 || a.gohooks == nil {
		return
	}
	for _, hook := range preHooks {
		if hook != nil {
			a.gohooks.AddPreTool(hook)
		}
	}
}

// inheritBorrowedTools copies only implementations absent from the child's
// own registry.  Built-ins and configured child tools stay authoritative even
// when a parent extension uses a colliding name (for example a project tool
// named Read).  The returned registry scope contains no disposer or parent
// pointer; the borrowed implementations remain reachable through the parent
// step lease until the child finishes.
func (a *Agent) inheritBorrowedTools(lease *tools.Lease) {
	if a == nil || lease == nil || a.registry == nil || a.childState == nil || a.childState.purpose != childPurposeTask {
		return
	}
	for _, name := range lease.Names() {
		if _, exists := a.registry.Get(name); exists {
			continue
		}
		tool, ok := lease.Get(name)
		if !ok || tool == nil {
			continue
		}
		a.registry.RegisterIn("inherited", tool)
		a.deferTools[name] = true
		if a.childState.allowed != nil {
			a.childState.allowed[name] = true
		}
	}
}

// childOptionsFromParent freezes the only mutable parent inputs consumed by a
// child. The provider constructor takes ownership of the copies and creates
// fresh session resources/identity around them.
func childOptionsFromParent(a *Agent, purpose string) (config.Config, Options, error) {
	if a == nil {
		return config.Config{}, Options{}, errors.New("agent: nil parent")
	}
	if purpose != childPurposeTask && purpose != childPurposeGuardian {
		return config.Config{}, Options{}, fmt.Errorf("agent: unknown child purpose %q", purpose)
	}
	cfg, binding, sourceModels, parentID, registryNames, perms, parentCtx := a.childSourceSnapshot()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	// Freeze the catalog after pinning the provider that was bound at the step
	// boundary. A registry route can be replaced while a request is streaming;
	// resolving the same model name from that newer route would silently send a
	// child through a different provider. Clone keeps the parent's immutable
	// snapshot intact, while Register/Route(Default) creates a child-owned
	// exact binding before the final Freeze.
	providers := pinChildProvider(sourceModels, cfg, binding)
	if perms == nil {
		perms = cfg.PermManager()
	} else {
		perms = perms.Clone()
	}
	if cfg.PermissionMode != string(permissions.ModePlan) {
		cfg.PermissionMode = string(perms.CurrentMode())
	}
	allowed := childAllowedTools(purpose, registryNames)
	if binding.model != "" {
		cfg.Model = binding.model
	}
	cfg.SessionID = ""
	cfg.EnableGuardian = config.BoolPtr(false)
	// The constructor filters cfg.Hooks to decision hooks. It must not replay
	// project lifecycle commands, start MCP servers, or write AutoMem state as
	// a startup side effect. Parent SubagentStart/Stop callbacks remain around
	// Task itself; they are not duplicated inside the child.
	cfg.MCPServers = nil
	cfg.EnableMemory = config.BoolPtr(false)
	return cfg, Options{
		RootContext:           parentCtx,
		EffectiveConfigFrozen: true,
		ProviderRegistry:      providers,
		Permissions:           perms,
		NonInteractive:        true,
		AllowedTools:          allowed,
		Purpose:               purpose,
		ParentSessionID:       parentID,
	}, nil
}

// childSourceSnapshot captures the immutable parent state a child inherits at a
// step boundary (or, for direct constructor callers, at the call boundary):
// config, active binding, frozen catalog, tool-name set, permissions, and the
// context the child runs under.
func (a *Agent) childSourceSnapshot() (cfg config.Config, binding modelBinding, sourceModels *plugin.ModelRegistry, parentID string, registryNames []string, perms *permissions.Manager, parentCtx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg = cloneConfig(a.cfg)
	binding = a.activeBinding
	parentID = a.sessionID
	// A child launched by a tool inherits the exact step admission boundary,
	// not whichever model/config/catalog happens to be live after a reload.
	// The parent captures this snapshot in beginStep and clears it when the turn
	// ends; the turn check prevents reuse by a later operation.
	if a.busy && a.childStep != nil && a.childStep.turn == a.turnSeq {
		step := a.childStep
		cfg = cloneConfig(&step.cfg)
		binding = step.binding
		sourceModels = step.models
		registryNames = append([]string(nil), step.registryNames...)
	} else {
		if binding.model != "" {
			cfg.Model = binding.model
		}
		if a.models != nil {
			// Direct constructor callers may not have gone through a running
			// step; still freeze the catalog at this boundary.
			sourceModels = a.models.Freeze()
		}
		if a.registry != nil {
			registryNames = a.registry.Names()
		}
	}
	parentCtx = a.turnCtx
	if parentCtx == nil {
		parentCtx = a.rootCtx
	}
	perms = a.perms
	return cfg, binding, sourceModels, parentID, registryNames, perms, parentCtx
}

// childAllowedTools computes the tool-name set a child may call for its
// purpose: task children drop the Task/Agent tools, guardians are restricted to
// the built-in read-only set.
func childAllowedTools(purpose string, registryNames []string) map[string]bool {
	allowed := make(map[string]bool, len(registryNames))
	for _, name := range registryNames {
		allowed[name] = true
	}
	if purpose == childPurposeTask {
		delete(allowed, "Task")
		delete(allowed, "Agent")
	}
	if purpose == childPurposeGuardian {
		allowed = make(map[string]bool, len(guardianReadOnlyTools))
		for name := range guardianReadOnlyTools {
			allowed[name] = true
		}
	}
	return allowed
}
