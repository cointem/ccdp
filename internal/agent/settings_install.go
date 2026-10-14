package agent

import (
	"context"
	"fmt"
	"strings"

	"ccdp/internal/mcp"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

// settingsCandidateInstall owns the complete prepared catalog generation. The
// originating worker retains candidate cleanup until this result is delivered.
type settingsCandidateInstall struct {
	ctx                                   context.Context
	candidate                             *settingsCandidate
	binding                               modelBinding
	settings                              session.Settings
	workflow                              session.WorkflowState
	nextRevision                          uint64
	planMode                              bool
	permissionMode                        permissions.Mode
	policyTightened, sandboxPolicyChanged bool
	mcpCandidate                          *mcp.RefreshCandidate
	manager                               *mcp.Manager
	capabilityRevision                    uint64
	result                                chan error
}

func (settingsCandidateInstall) ownerControlMessage() {}

func (a *Agent) installSettingsCandidate(input settingsCandidateInstall) error {
	if err := input.ctx.Err(); err != nil {
		return err
	}
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()
	a.mu.Lock()
	stale := a.sequences.settingsRev != input.candidate.sourceSettingsRevision || a.services.mcp != input.manager || a.capabilities.capabilityRevision != input.capabilityRevision
	if a.services.sandbox != nil && a.services.sandbox.Snapshot().Revision != input.candidate.sourceSandboxRevision {
		stale = true
	}
	closing := a.lifecycle.closing || a.lifecycle.closed
	a.mu.Unlock()
	if closing {
		return fmt.Errorf("session closed before settings installation")
	}
	if stale {
		return fmt.Errorf("settings changed while candidate was being prepared")
	}
	candidate, binding, settings, workflow := input.candidate, input.binding, input.settings, input.workflow
	nextRevision, mcpCandidate := input.nextRevision, input.mcpCandidate
	planMode, permissionMode := input.planMode, input.permissionMode
	policyTightened, sandboxPolicyChanged := input.policyTightened, input.sandboxPolicyChanged
	// SettingsChanged/WorkflowChanged are committed before any executable
	// extension is made visible. A journal failure therefore cannot leave a
	// live candidate whose durable state says the old settings were active.
	if err := a.persistSettingsWorkflowFact(settings, nextRevision, &workflow); err != nil {
		return err
	}

	if mcpCandidate != nil {
		if err := mcpCandidate.Publish(); err != nil {
			// refreshMu makes this impossible while the Agent owns the candidate,
			// but surface it as a hard failure rather than claiming a publication.
			return fmt.Errorf("commit MCP candidate: %w", err)
		}

	}
	a.services.mcp.SetSandbox(candidate.sandbox)
	// A generated HTTP binding may have a new endpoint/key or a newly wrapped
	// capability ceiling. Publish it into the mutable registry only after the
	// durable settings admission; explicit plugin routes remain owned by their
	// plugin and are represented by the binding itself.
	if binding.client != nil && binding.routeKind == "http" && a.services.models != nil {
		a.services.models.Register(binding.client)
		a.services.models.RouteDefault(binding.model, binding.client.Name(),
			binding.httpBinding(candidate.cfg.ResolveProvider(binding.model).APIKey))
	}

	// Registry registration is infallible after validation. Publish each
	// project-owned scope as one complete batch; built-ins/plugin scopes and any
	// frozen step leases remain untouched until their turn ends.
	customTools := make([]tools.Tool, 0, len(candidate.tools))
	customPerms := make(map[string]string, len(candidate.customPerms))
	for _, spec := range candidate.tools {
		name := strings.TrimSpace(spec.Name)
		if name == "" || strings.TrimSpace(spec.Command) == "" {
			continue
		}
		customTools = append(customTools, tools.NewCommandTool(name, spec.Description, spec.Command, spec.InputSchema))
		customPerms[name] = candidate.customPerms[name]
	}
	webTools := make([]tools.Tool, 0, 2)
	if candidate.webTools {
		webTools = append(webTools, tools.NewWebFetchTool(), tools.NewWebSearchTool())
	}
	a.services.registry.ReplaceScopes(map[string][]tools.Tool{"custom": customTools, "web": webTools})
	a.mu.Lock()
	a.discovery.customPerms = customPerms
	*a.services.cfg = candidate.cfg
	if candidate.workspaceChange {
		a.services.baseCfg.Workspace = candidate.cfg.Workspace
	}
	if binding.client != nil {
		a.modelsState.activeBinding = binding
		a.services.client = binding.client
		a.modelsState.primaryBinding = binding
		a.modelsState.activeModel = binding.model
	}
	a.services.sandbox = candidate.sandbox
	a.plan.planMode = planMode
	a.plan.planBaseMode = permissionMode
	a.lifecycle.workflow = protocol.WorkflowState(workflow.Phase)
	a.services.cfg.PermissionMode = string(permissionMode)
	a.sequences.settingsRev = nextRevision
	if policyTightened {
		clear(a.capabilities.capabilityGrants)
		a.capabilities.capabilityRevision++
		a.capabilities.capabilityRevoking = false
	} else if sandboxPolicyChanged {
		a.capabilities.capabilityRevision++
	}
	resources := a.services.resources
	a.mu.Unlock()
	if policyTightened && resources != nil && resources.Processes != nil {
		resources.Processes.ResumeAfterRevocation()
	}
	a.services.perms.SetMode(permissionMode)
	a.services.perms.SetPolicy(candidate.cfg.PermissionPolicy())
	a.services.hooks.Update(cloneHookConfig(candidate.hooks))
	a.services.hooks.SetSandbox(candidate.sandbox)
	// Keep checkpoint git operations on the newly published boundary after a
	// reload or /cd. Typed operations still pass their own cancellable context
	// through CreateContext/RestoreContext below.
	a.syncHookContext()

	return nil
}
