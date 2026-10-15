package agent

import (
	"context"
	"encoding/json"
	"sort"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
)

// Snapshot returns a deep, credential-free view of runtime state.
func (a *Agent) Snapshot(ctx context.Context) (protocol.SessionView, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return protocol.SessionView{}, ctx.Err()
	default:
	}
	if view := a.publishedSnapshot.Load(); view != nil {
		return protocol.CloneSessionView(*view), nil
	}
	// An unstarted session still belongs to construction. Its first owner
	// publication switches readers permanently to immutable snapshots.
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked(), nil
}

func (a *Agent) snapshotLocked() protocol.SessionView {
	view := protocol.SessionView{Catalog: a.catalogSnapshotLocked()}
	view.ApplyState(a.stateLocked())
	view.History = make([]protocol.MessageView, 0, len(a.historyLocked()))
	for _, m := range a.historyLocked() {
		mv := protocol.MessageView{InternalSource: m.InternalSource, ID: m.ID, Role: string(m.Role), Content: m.Content, ReasoningContent: m.ReasoningContent}
		for _, tc := range m.ToolCalls {
			mv.ToolCallIDs = append(mv.ToolCallIDs, protocol.CallID(tc.ID))
			mv.ToolCalls = append(mv.ToolCalls, protocol.MessageViewToolCall{ID: tc.ID, Name: tc.Name})
		}
		view.History = append(view.History, mv)
	}
	return view
}

func (a *Agent) stateLocked() protocol.SessionState {
	settings := a.settingsSnapshotLocked()
	view := protocol.SessionState{SessionID: protocol.SessionID(a.conversation.sessionID), Revision: a.revisionLocked(),
		Phase: a.lifecycle.phase, Workflow: a.lifecycle.workflow, Busy: a.turn.busy, Closing: a.lifecycle.closing,
		Settings: settings, Usage: protocol.UsageSnapshot{
			InputTokens: a.runtimeUsageLocked().InputTokens, OutputTokens: a.runtimeUsageLocked().OutputTokens,
			CachedTokens: a.runtimeUsageLocked().CachedTokens, Cache: a.runtimeUsageLocked().Cache, TurnCount: a.runtimeUsageLocked().TurnCount,
		}}
	view.HistoryLength = len(a.historyLocked())
	if a.services.supervisor != nil && a.services.supervisor.root == a {
		view.OutstandingWork = int(a.services.supervisor.directory.Outstanding())
	}
	if a.turn.busy {
		view.WorkStartedAt = a.turn.workStartedAt
		view.WorkLabel = a.turn.workLabel
	}
	view.Goal = protocol.CloneGoal(a.conversation.durable.Goal)
	view.SandboxRuntime.NetworkProxy = a.services.cfg.NetworkProxy
	view.SandboxRuntime.RevocationPending = a.capabilities.capabilityRevoking
	if a.services.sandbox != nil {
		view.SandboxRuntime.ExternalExecutionPossible = a.services.sandbox.ExternalExecutionPossible()
	}
	for _, capability := range a.capabilities.capabilityGrants {
		view.SandboxRuntime.SessionGrants = append(view.SandboxRuntime.SessionGrants, capability)
	}
	sort.Slice(view.SandboxRuntime.SessionGrants, func(i, j int) bool {
		return capabilityKey(view.SandboxRuntime.SessionGrants[i]) < capabilityKey(view.SandboxRuntime.SessionGrants[j])
	})
	view.PendingInputs = append([]protocol.InputView(nil), a.conversation.durable.PendingInputViews()...)
	if a.decisions.pendingApproval != nil {
		view.Approval = &protocol.ApprovalView{ID: a.decisions.pendingApproval.ID, Tool: a.decisions.pendingApproval.Tool,
			Reason: a.decisions.pendingApproval.Reason, Args: approvalArgsView(a.decisions.pendingApproval), Capabilities: append([]protocol.CapabilityRequest(nil), a.decisions.pendingApproval.Capabilities...)}
	}
	view.Question = protocol.CloneQuestionRequest(a.decisions.pendingQuestion)
	if a.plan.pendingPlan != nil {
		view.Plan = &protocol.PlanView{ID: a.plan.pendingPlan.ID, Text: a.plan.pendingPlan.Plan, Version: planViewVersion}
	}
	if a.lifecycle.lastTurn != nil {
		outcome := *a.lifecycle.lastTurn
		view.LastTurn = &outcome
	}
	view.Tasks = a.tasksViewLocked()
	used, messages, tools := a.contextTokenBreakdownLocked()
	view.ContextUsedTokens = used
	view.MessagesTokens = messages
	view.ToolsTokens = tools
	view.SystemTokens = max(0, used-messages-tools)
	view.Transcript, view.TranscriptMore = a.transcript.Snapshot()
	return view
}

// tasksViewLocked projects the session's working task list for rendering. The
// store keeps its own mutex, so Snapshot is safe under a.mu. It returns nil
// before resources are acquired or once the store is closed, leaving Tasks
// omitted from the view.
func (a *Agent) tasksViewLocked() []protocol.TaskView {
	if a.services.resources == nil || a.services.resources.Todos == nil {
		return nil
	}
	states, err := a.services.resources.Todos.Snapshot()
	if err != nil {
		return nil
	}
	if len(states) == 0 {
		return nil
	}
	tasks := make([]protocol.TaskView, 0, len(states))
	for _, st := range states {
		tasks = append(tasks, protocol.TaskView{ID: st.ID, Title: st.Content, Status: st.Status})
	}
	return tasks
}

// contextUsedTokensLocked mirrors estimateTokens while retaining the snapshot
// lock. Calling estimateTokens here would try to acquire a.mu recursively and
// would make Snapshot deadlock.
// historyTokenPrefixLocked caches token estimates for committed history. Each
// entry is immutable: append/rewrite/compaction replace slices or append entries.
// State-only changes reuse the totals without walking old messages.
func (a *Agent) historyTokenPrefixLocked() []int {
	var first *messages.Message
	if len(a.historyLocked()) > 0 {
		first = &a.historyLocked()[0]
	}
	cache := &a.historyTokenCache
	if cache.first != first || len(cache.prefix) > len(a.historyLocked())+1 {
		cache.first, cache.prefix = first, []int{0}
	}
	if len(cache.prefix) == 0 {
		cache.prefix = []int{0}
	}
	for i := len(cache.prefix) - 1; i < len(a.historyLocked()); i++ {
		cache.prefix = append(cache.prefix, cache.prefix[len(cache.prefix)-1]+estimateMessageTokens(a.historyLocked()[i]))
	}
	return cache.prefix
}

func (a *Agent) contextUsedTokensLocked() int {
	prefix := a.historyTokenPrefixLocked()
	total := prefix[len(prefix)-1]
	base, baseLen := a.turnMemory.tokenBaseline.promptTokens, a.turnMemory.tokenBaseline.historyLen
	if base > 0 && baseLen >= 0 && baseLen <= len(a.historyLocked()) {
		return base + total - prefix[baseLen]
	}
	return total
}

func (a *Agent) contextTokenBreakdownLocked() (used, messageTokens, toolTokens int) {
	prefix := a.historyTokenPrefixLocked()
	used = a.contextUsedTokensLocked()
	messageTokens = min(prefix[len(prefix)-1], used)
	toolTokens = min(a.toolsTokensEstimate, max(0, used-messageTokens))
	return used, messageTokens, toolTokens
}

func approvalArgsView(req *ApprovalRequest) json.RawMessage {
	if req == nil || req.Command == "" {
		return nil
	}
	if json.Valid([]byte(req.Command)) {
		return append(json.RawMessage(nil), req.Command...)
	}
	b, err := json.Marshal(map[string]string{"command": req.Command})
	if err != nil {
		return nil
	}
	return b
}

func (a *Agent) settingsSnapshotLocked() protocol.SettingsSnapshot {
	b := a.modelsState.activeBinding
	return protocol.SettingsSnapshot{Revision: a.sequences.settingsRev, Workspace: a.services.cfg.Workspace,
		Model:         protocol.ModelBinding{Model: b.model, Provider: b.provider, Endpoint: b.endpoint, BindingVersion: b.version},
		ExecutionMode: map[bool]protocol.ExecutionMode{true: protocol.ExecutionModePlan, false: protocol.ExecutionModeExecute}[a.plan.planMode],
		Permission:    protocol.PermissionPolicy{Mode: string(a.services.perms.CurrentMode()), AlwaysAllow: append([]string(nil), a.services.cfg.AlwaysAllow...), AlwaysDeny: append([]string(nil), a.services.cfg.AlwaysDeny...), Revision: a.sequences.settingsRev},
		Sandbox: protocol.SandboxPolicy{NetworkAccess: a.services.cfg.NetworkAccess,
			AdditionalDirectories:         append([]string(nil), a.services.cfg.AdditionalDirectories...),
			AdditionalReadOnlyDirectories: append([]string(nil), a.services.cfg.AdditionalReadOnlyDirectories...),
			DisallowedDirectories:         append([]string(nil), a.services.cfg.DisallowedDirectories...), Revision: a.sequences.settingsRev},
		ReasoningEffort: a.services.cfg.ReasoningEffortFor(a.activeModelLocked()), Verbosity: a.services.cfg.Verbosity,
		ContextWindow: a.contextWindowLocked(), CompactThreshold: a.services.cfg.CompactThreshold, MaxOutputTokens: a.services.cfg.MaxOutputTokensFor(a.activeModelLocked()), MaxTurns: a.services.cfg.MaxTurns}
}

// defaultProviderID names the unscoped top-level base_url/api_key pair in the
// catalog, the fallback for models no configured [providers] record serves.
const defaultProviderID = "default"

func (a *Agent) catalogSnapshotLocked() protocol.CatalogSnapshot {
	out := protocol.CatalogSnapshot{Version: a.sequences.catalogVersion}
	// The catalog is what /model offers. A configured provider record declares
	// the models it serves and owns their endpoint and credential, so the two
	// can never disagree in the picker. The unscoped top-level base_url has no
	// declared list, so the model currently bound to it is published under
	// defaultProviderID to keep the active selection visible.
	entries := make(map[string]*protocol.ProviderSnapshot, len(a.services.cfg.Providers)+1)
	publish := func(id string, models ...string) {
		if id == "" {
			id = defaultProviderID
		}
		entry, ok := entries[id]
		if !ok {
			entry = &protocol.ProviderSnapshot{ID: id, Version: a.sequences.catalogVersion}
			entries[id] = entry
		}
		for _, model := range models {
			if model != "" && !containsModel(entry.Models, model) {
				entry.Models = append(entry.Models, model)
			}
		}
	}
	for id, provider := range a.services.cfg.Providers {
		if provider.ModelConfigs != nil {
			for model := range provider.ModelConfigs {
				publish(id, id+"/"+model)
			}
		} else {
			publish(id, provider.Models...)
		}
	}
	// Switching the active model must not remove the configured top-level model
	// from the picker, even when the destination is listed by another provider.
	if a.configuredModel != "" {
		resolved := a.services.cfg.ResolveProvider(a.configuredModel)
		publish(resolved.ID, a.configuredModel)
	}
	if a.modelsState.activeBinding.model != "" {
		switch {
		case a.modelsState.activeBinding.providerID != "":
			publish(a.modelsState.activeBinding.providerID, a.modelsState.activeBinding.model)
		case a.modelsState.activeBinding.routeKind != "" && a.modelsState.activeBinding.routeKind != "http":
			publish(a.modelsState.activeBinding.provider, a.modelsState.activeBinding.model)
		default:
			publish(defaultProviderID, a.modelsState.activeBinding.model)
		}
	}
	for _, entry := range entries {
		sort.Strings(entry.Models)
		out.Providers = append(out.Providers, *entry)
	}
	lease := a.services.registry.Acquire()
	defer lease.Close()
	for _, name := range lease.Names() {
		schema, ok := lease.SchemaJSON(name)
		if !ok {
			continue
		}
		out.Tools = append(out.Tools, protocol.ToolSnapshot{ID: name, Schema: schema})
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].ID < out.Providers[j].ID })
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].ID < out.Tools[j].ID })
	return out
}

func containsModel(models []string, want string) bool {
	for _, model := range models {
		if model == want {
			return true
		}
	}
	return false
}

func marshalSchema(v map[string]any) []byte {
	b, _ := jsonMarshal(v)
	return b
}

// jsonMarshal is a variable-sized helper kept here so snapshot construction
// never stores a map owned by the registry. (The actual encoding is local.)
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// Called with mu held. Catalog replacement is independent of the
// frequent state payload and only travels when settings or registered tools change.
func (a *Agent) catalogUpdateLocked() *protocol.CatalogSnapshot {
	toolsVersion := a.services.registry.Version()
	prior := a.publishedCatalog
	if prior.catalog == a.sequences.catalogVersion && prior.settings == a.sequences.settingsRev && prior.tools == toolsVersion {
		return nil
	}
	a.refreshCatalogLocked()
	copy := a.snapshotProjection.catalog
	a.publishedCatalog.catalog, a.publishedCatalog.settings, a.publishedCatalog.tools = a.sequences.catalogVersion, a.sequences.settingsRev, toolsVersion
	return &copy
}
