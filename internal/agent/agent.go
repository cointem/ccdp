package agent

import (
	"ccdp/internal/protocol"
	"context"
	cryptorand "crypto/rand"
	"os"
	"sync"
	"sync/atomic"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/sandbox"
	"ccdp/internal/sessionview"
)

// Agent composes session services and the command owner. Business transitions
// enter through typed owner messages; construction initializes these values
// exclusively before publication. mu protects read views and producer inboxes.
type Agent struct {
	persistenceFault   atomic.Pointer[error]
	publishedSnapshot  atomic.Pointer[protocol.SessionView]
	snapshotProjection snapshotProjection
	services           sessionServices
	owner              *sessionOwner
	conversation       conversationState
	lifecycle          sessionLifecycle
	turn               turnLifecycleData
	decisions          approvalState
	capabilities       capabilityState
	plan               planState
	discovery          toolDiscoveryData
	modelsState        modelState
	budget             budgetState
	turnMemory         turnMemoryState
	sequences          seqState
	inbox              dedupState
	publications       watchState
	children           childRuntimeBindings

	constructionComplete     bool   // immutable once the constructor returns
	configuredModel          string // launch model, independent of the active selection
	catalogNotificationsDone <-chan struct{}
	memorySessions           *memorySessionCatalog
	finalization             *childFinalization // admission shares the close boundary under mu
	streamEpoch              string
	transcript               *sessionview.Transcript
	views                    *sessionview.Hub // owner access only

	// Readers observe config, catalog and policy from the same published generation.
	settingsCommitMu sync.RWMutex
	mu               sync.Mutex

	toolUses            int
	toolsTokensEstimate int
	publishedCatalog    struct{ catalog, settings, tools uint64 } // owner access only
	historyTokenCache   struct {
		first  *messages.Message
		prefix []int
	}
	trace   *os.File
	traceMu sync.Mutex
}

// AddDirectory grants the sandbox access to an extra directory at runtime.
func (a *Agent) AddDirectory(dir string) { a.setDirectoryAccess(dir, false) }

// AddDisallowedDirectory blocks a directory at runtime.
func (a *Agent) AddDisallowedDirectory(dir string) { a.setDirectoryAccess(dir, true) }

// SkillNames returns the loaded skill names (used by /skills).
func (a *Agent) SkillNames() []string {
	all := a.services.skills.All()
	names := make([]string, 0, len(all))
	for _, s := range all {
		names = append(names, s.Name)
	}
	return names
}

// PluginNames returns the loaded plugin names in load order.
func (a *Agent) PluginNames() []string { return a.services.host.Names() }

// HostPending returns registered plugin names waiting for their dependencies.
func (a *Agent) HostPending() []string { return a.services.host.Pending() }

// MCPServers returns the connected MCP server names.
func (a *Agent) MCPServers() []string { return a.services.mcp.Names() }

// MCPInfo returns connected MCP server name → advertised tool names.
func (a *Agent) MCPInfo() map[string][]string { return a.services.mcp.ToolNames() }

// MCPResources returns MCP server name → advertised resource URIs.
func (a *Agent) MCPResources() map[string][]string {
	out := map[string][]string{}
	ctx := context.Background()
	for _, name := range a.services.mcp.Names() {
		c, ok := a.services.mcp.Server(name)
		if !ok {
			continue
		}
		rs, err := c.Resources(ctx)
		if err != nil {
			continue
		}
		for _, r := range rs {
			out[name] = append(out[name], r.URI)
		}
	}
	return out
}

// MCPPrompts returns MCP server name → advertised prompt names.
func (a *Agent) MCPPrompts() map[string][]string {
	out := map[string][]string{}
	ctx := context.Background()
	for _, name := range a.services.mcp.Names() {
		c, ok := a.services.mcp.Server(name)
		if !ok {
			continue
		}
		ps, err := c.Prompts(ctx)
		if err != nil {
			continue
		}
		for _, p := range ps {
			out[name] = append(out[name], p.Name)
		}
	}
	return out
}

// PermissionMode returns the current permission mode.
func (a *Agent) PermissionMode() permissions.Mode { return a.services.perms.CurrentMode() }

// syncHookContext keeps lifecycle and tool hook payloads aligned with mutable
// agent state after mode, workspace or session changes.
func (a *Agent) syncHookContext() {
	a.mu.Lock()
	sessionID, ws := a.conversation.sessionID, a.services.cfg.Workspace
	a.mu.Unlock()
	a.services.hooks.SetContext(sessionID, ws, string(a.services.perms.CurrentMode()))
}

// Usage returns a snapshot of the session's token accounting.
func (a *Agent) Usage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runtimeUsageLocked()
}

// ToolUses returns how many tool calls reached execution this session.
func (a *Agent) ToolUses() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.toolUses
}

// HooksList returns the configured hooks grouped by event.
func (a *Agent) HooksList() map[string][]string { return a.services.hooks.List() }

// PlanMode reports whether plan mode is active.
func (a *Agent) PlanMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.plan.planMode
}

// SessionID returns the session identifier.
func (a *Agent) SessionID() string { return a.conversation.sessionID }

// WorkspaceLabel returns the absolute workspace path.
func (a *Agent) WorkspaceLabel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services.cfg.Workspace
}

// SessionDir returns the directory where sessions are persisted.
func (a *Agent) SessionDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services.cfg.SessionDir
}

// currentProvider returns the provider currently in use (primary or fallback).
// Safe for concurrent use with SetModel and the fallback switch.
func (a *Agent) currentProvider() llm.Provider {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.modelsState.activeBinding.client != nil {
		return a.modelsState.activeBinding.client
	}
	return a.services.client
}

// activeModelSnapshot returns the model name requests should carry: the
// fallback model after a fallback switch, otherwise the configured primary.
// Safe for concurrent use with SetModel and the fallback switch.
func (a *Agent) activeModelSnapshot() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.modelsState.activeBinding.model != "" {
		return a.modelsState.activeBinding.model
	}
	if a.modelsState.activeModel != "" {
		return a.modelsState.activeModel
	}
	return a.services.cfg.Model
}

// currentSandbox returns the sandbox in use. The pointer is swapped by
// SetWorkspace (UI goroutine), so tools and turn goroutines must not read the
// field directly.
func (a *Agent) currentSandbox() *sandbox.Sandbox {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services.sandbox
}

func newSessionID() string {
	// Session IDs are also directory names and can be generated concurrently
	// by independent processes. A cryptographically random base32 value avoids
	// the microsecond timestamp collisions of the old format without adding a
	// process-local counter that would still collide across processes.
	return cryptorand.Text()
}
