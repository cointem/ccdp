package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/skills"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

func (a *Agent) applySetModel(cmd protocol.Command, owned ...bool) protocol.Receipt {
	requested := strings.TrimSpace(cmd.Model.Model)
	return a.applySettingsCommand(cmd, func(target *session.Settings) error {
		target.Model = requested
		return nil
	}, owned...)
}

// registerBindingRoutes publishes the resolved client and its route on the
// model registry so the binding stays consistent for later requests.
func (a *Agent) registerBindingRoutes(binding modelBinding, cfg config.Config) {
	a.services.models.Register(binding.client)
	if binding.routeKind == "http" {
		a.services.models.RouteDefault(binding.model, binding.provider,
			binding.httpBinding(cfg.ResolveProvider(binding.model).APIKey))
	} else {
		a.services.models.Route(binding.model, binding.provider)
	}
}

func (a *Agent) applyBindingLocked(binding modelBinding) {
	a.modelsState.activeBinding = binding
	a.services.client = binding.client
	a.modelsState.primaryBinding = binding
	a.modelsState.activeModel = binding.model
	a.services.cfg.Model = binding.model
	a.services.baseCfg.Model = binding.model
	a.conversation.modelSwitchMsg = "# Model\nYou have been switched to the model \"" + binding.model + "\". Adjust your responses to its capabilities."
}

func (a *Agent) beginStep() stepRuntime {
	step, _ := a.beginStepChecked()
	return step
}

// beginStepChecked captures the step runtime that becomes visible to the next
// provider request. It holds settingsCommitMu so a step boundary never observes a
// half-applied settings change racing on the command loop.
func (a *Agent) beginStepChecked() (stepRuntime, error) {
	step, err := a.requestStepRuntime()
	if err != nil {
		return stepRuntime{}, err
	}
	if err := a.loadStepExternalContext(&step); err != nil {
		return stepRuntime{}, err
	}
	return step, nil
}

func (a *Agent) captureStepOwned() (stepRuntime, error) {
	a.settingsCommitMu.Lock()
	a.mu.Lock()
	revoking := a.capabilities.capabilityRevoking
	closing := a.lifecycle.closing || a.lifecycle.closed
	a.mu.Unlock()
	if revoking {
		a.settingsCommitMu.Unlock()
		return stepRuntime{}, errors.New("sandbox capability revocation is in progress")
	}
	if closing {
		a.settingsCommitMu.Unlock()
		return stepRuntime{}, errors.New("session is closing")
	}
	step, lease := a.captureStepRuntime()
	step.tools = a.toolDefsSnapshotFromLease(lease)
	if err := a.recordStepTools(&step); err != nil {
		releaseStepLease(step)
		a.clearStepContext(step.turn, step.step)
		a.settingsCommitMu.Unlock()
		return stepRuntime{}, err
	}
	step.goalPrompt = a.goalPrompt()
	a.mu.Lock()
	a.toolsTokensEstimate = estimateToolsTokens(step.tools)
	a.mu.Unlock()
	a.settingsCommitMu.Unlock()
	return step, nil
}

// captureStepRuntime snapshots the step along with every mutable parent input a
// Task/guardian can inherit, freezing the registry and tool lease at the same
// lock boundary as cfg/provider/history. Registry reads are taken once here so a
// plugin/MCP update during streaming cannot mix a new schema with this step's
// binding.
func (a *Agent) captureStepRuntime() (stepRuntime, *tools.Lease) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sequences.stepSeq++
	a.conversation.stepGoalID = ""
	if a.conversation.durable.Goal != nil {
		a.conversation.stepGoalID = a.conversation.durable.Goal.ID
	}
	var lease *tools.Lease
	var mcpLease *mcp.StepLease
	if a.services.mcp != nil {
		mcpLease = a.services.mcp.AcquireStep(a.services.registry)
		lease = mcpLease.Tools
	} else if a.services.registry != nil {
		// Capture implementations at the same boundary as cfg/provider/history.
		// The lease remains valid even if a plugin or MCP server replaces its
		// visible name while this request is streaming.
		lease = a.services.registry.Acquire()
	}
	step := stepRuntime{binding: a.modelsState.activeBinding, cfg: cloneConfig(a.services.cfg), history: messages.Clone(a.historyLocked()),
		turn: a.sequences.turnSeq, step: a.sequences.stepSeq, settingsRev: a.sequences.settingsRev, contextRev: a.sequences.contextRev,
		catalogVersion: a.sequences.catalogVersion, sessionID: a.conversation.sessionID, sessionStartAt: a.conversation.sessionStartAt,
		skillStore: a.services.skills, isolated: a.children.childState != nil, plan: a.plan.planMode, modelSwitchMsg: a.conversation.modelSwitchMsg, interruptNote: a.turnMemory.interruptNote, toolLease: lease, mcpLease: mcpLease}
	step.policy = a.services.sandbox.Snapshot()
	var frozenModels *plugin.ModelRegistry
	if a.services.models != nil {
		frozenModels = a.services.models.Freeze()
	}
	var registryNames []string
	if lease != nil {
		registryNames = lease.Names()
	}
	a.children.childStep = &childStepSnapshot{
		mcpLease:      mcpLease,
		cfg:           cloneConfig(&step.cfg),
		binding:       step.binding,
		models:        frozenModels,
		registryNames: append([]string(nil), registryNames...),
		toolLease:     lease,
		preHooks: func() []plugin.PreToolHook {
			if a.services.gohooks == nil {
				return nil
			}
			return a.services.gohooks.SnapshotPreTool()
		}(),
		turn:     step.turn,
		step:     step.step,
		revision: step.settingsRev,
	}
	a.conversation.modelSwitchMsg = ""
	a.turnMemory.interruptNote = false
	return step, lease
}

// loadStepExternalContext captures the instructions, skill index, and tool
// definitions at the step boundary so a file edit during provider preparation
// cannot change the request after admission. An input-load failure releases the
// frozen state and is reported as a user-retryable stepInputError.
func (a *Agent) loadStepExternalContext(step *stepRuntime) error {
	instructions, inputErr := workspace.LoadInstructionsChecked(step.cfg.Workspace)
	if inputErr != nil {
		a.releaseStepContext(*step)
		return &stepInputError{err: fmt.Errorf("load project instructions: %w", inputErr)}
	}
	step.instructions = instructions
	skillStore := step.skillStore
	if skillStore != nil && !step.isolated {
		home, _ := os.UserHomeDir()
		if inputErr := skillStore.LoadChecked(filepath.Join(home, ".ccdp", "skills"), filepath.Join(step.cfg.Workspace, ".ccdp", "skills")); inputErr != nil {
			a.releaseStepContext(*step)
			return &stepInputError{err: fmt.Errorf("load skills: %w", inputErr)}
		}
		step.skillsSection = skillStore.SkillsSection()
	}
	step.systemBlocks = a.buildSystemBlocks(step.cfg, step.sessionStartAt, step.sessionID, step.plan, step.modelSwitchMsg, step.interruptNote, step.instructions, step.skillsSection, step.history, step.policy)
	if step.goalPrompt != "" {
		step.systemBlocks = append(step.systemBlocks, llm.SystemBlock{Text: step.goalPrompt})
	}
	return nil
}

// releaseStepContext releases a step's frozen lease and clears its child-session
// snapshot on a failed step admission.
func (a *Agent) releaseStepContext(step stepRuntime) {
	releaseStepLease(step)
	if a.owner == nil {
		a.clearStepContext(step.turn, step.step)
		return
	}
	request := stepReleased{turn: step.turn, step: step.step, done: make(chan struct{})}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return
	}
	select {
	case <-request.done:
	case <-a.owner.done:
	}

}

type stepRuntime struct {
	policy         *sandbox.Sandbox
	goalPrompt     string
	systemBlocks   []llm.SystemBlock
	skillStore     *skills.Store
	isolated       bool
	binding        modelBinding
	cfg            config.Config
	history        []messages.Message
	turn           uint64
	step           uint64
	settingsRev    uint64
	contextRev     uint64
	catalogVersion uint64
	sessionID      string
	sessionStartAt time.Time
	plan           bool
	modelSwitchMsg string
	interruptNote  bool
	instructions   string
	skillsSection  string
	tools          []llm.ToolDef
	toolLease      *tools.Lease
	mcpLease       *mcp.StepLease
}

// stepInputError marks a bounded external prompt input (instructions, skill
// metadata, or a frozen attachment) that cannot be admitted for this request.
// It is a user-retryable input problem, not evidence that the session log is
// corrupt; callers must fail only this turn and leave the persistence gate
// usable for a subsequent fix/retry.
type stepInputError struct{ err error }

func (e *stepInputError) Error() string {
	if e == nil || e.err == nil {
		return "step input is unavailable"
	}
	return e.err.Error()
}

func (e *stepInputError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// buildRequestForStep forces the request model to the binding captured before
// context construction.
func (a *Agent) buildRequestForStep(step stepRuntime) llm.CompletionRequest {
	req := a.buildRequestFromConfig(step.cfg, step.binding.model, joinSystemBlocks(step.systemBlocks), step.history, step.plan, step.tools)
	req.SystemBlocks = append([]llm.SystemBlock(nil), step.systemBlocks...)
	return req
}

// buildRequestForStepChecked verifies every frozen image before constructing a
// provider request. A canonical input that references a missing/corrupt blob
// is a durable-integrity failure, not a reason to silently send the markdown
// marker and pretend the model saw the image.
func (a *Agent) buildRequestForStepChecked(step stepRuntime) (llm.CompletionRequest, error) {
	if _, err := a.todoSectionChecked(); err != nil {
		return llm.CompletionRequest{}, &stepInputError{err: fmt.Errorf("load task state: %w", err)}
	}
	var imageBytes int64
	for _, message := range step.history {
		if message.Role != messages.RoleUser || !message.ImagesFrozen {
			continue
		}
		_, used, _, err := a.imageContentPartsForMessage(step.cfg.Workspace, message, maxAttachmentBytes-imageBytes)
		if err != nil {
			return llm.CompletionRequest{}, &stepInputError{err: fmt.Errorf("frozen input image: %w", err)}
		}
		imageBytes += used
	}
	return a.buildRequestForStep(step), nil
}

// toolDefsSnapshot converts one registry view into provider-neutral wire
// declarations. The discovered/deferred decision is copied before reading the
// registry so the callback never re-enters Agent.mu.
func (a *Agent) toolDefsSnapshot() []llm.ToolDef {
	if a.services.registry == nil {
		return nil
	}
	return a.toolDefsSnapshotFromLease(a.services.registry.Acquire())
}

func (a *Agent) toolDefsSnapshotFromLease(lease *tools.Lease) []llm.ToolDef {
	if lease == nil {
		return nil
	}
	a.mu.Lock()
	deferred := make(map[string]bool, len(a.discovery.deferTools))
	for name, value := range a.discovery.deferTools {
		deferred[name] = value
	}
	discovered := make(map[string]bool, len(a.discovery.discovered))
	for name, value := range a.discovery.discovered {
		discovered[name] = value
	}
	a.mu.Unlock()
	schemas := lease.SchemasFiltered(func(name string) bool {
		if a.children.childState != nil && a.children.childState.allowed != nil && !a.children.childState.allowed[name] {
			return false
		}
		return !deferred[name] || discovered[name]
	})
	defs := make([]llm.ToolDef, 0, len(schemas))
	for _, schema := range schemas {
		fn, ok := schema["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		description, _ := fn["description"].(string)
		parameters, _ := fn["parameters"].(map[string]any)
		if name == "" {
			continue
		}
		tool, _ := lease.Get(name)
		defs = append(defs, llm.ToolDef{Type: "function", PlanAllowed: planAllowedImplementation(tool), Function: llm.FuncDef{
			Name: name, Description: description, Parameters: cloneMap(parameters),
		}})
	}
	return defs
}
