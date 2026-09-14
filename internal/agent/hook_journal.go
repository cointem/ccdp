package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"ccdp/internal/hooks"
	"ccdp/internal/session"
)

const maxHookFactOutputBytes = 64 << 10

// hookInvocation is the identity of one actual hook-manager invocation. A
// stage can contain several configured shell hooks, but the manager executes
// them as one ordered decision boundary; recording that boundary before and
// after the call preserves the external side-effect ordering without making
// the session log depend on hook implementation details.
type hookInvocation struct {
	HookID   string
	EffectID string
	Stage    string
}

// runHookWithJournal admits the hook boundary before calling the external
// runner and commits its complete result before returning it to the caller.
// A start failure never invokes the hook. A finish failure returns a deny so a
// decision-affecting caller cannot continue on an unconfirmed hook outcome;
// both failures poison the session's durable-work gate.
func (a *Agent) runHookWithJournal(ctx context.Context, stage string, run func(context.Context) hooks.Output) hooks.Output {
	if run == nil {
		return hooks.Output{Decision: hooks.DecisionDeny, Reason: "hook runner is unavailable"}
	}
	if a == nil || a.hooks == nil || !a.hooks.Has(stage) {
		return run(ctx)
	}
	invocation := hookInvocation{HookID: nextRuntimeID("hook"), EffectID: nextRuntimeID("hook-effect"), Stage: stage}
	if err := a.persistHookStarted(invocation); err != nil {
		a.markPersistenceFailure(err)
		return hooks.Output{Decision: hooks.DecisionDeny, Reason: "hook was not durably admitted: " + err.Error()}
	}
	out := run(ctx)
	if err := a.persistHookFinished(ctx, invocation, out); err != nil {
		a.markPersistenceFailure(err)
		return hooks.Output{Decision: hooks.DecisionDeny, Reason: "hook result was not durably recorded: " + err.Error()}
	}
	return out
}

func (a *Agent) persistHookStarted(invocation hookInvocation) error {
	if invocation.HookID == "" || invocation.EffectID == "" || invocation.Stage == "" {
		return errors.New("agent: hook journal start identity is required")
	}
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: stableID("hook-started", invocation),
		Events:        []session.Event{session.HookStarted{HookID: invocation.HookID, EffectID: invocation.EffectID, Stage: invocation.Stage}},
	}); err != nil {
		return fmt.Errorf("agent: persist HookStarted: %w", err)
	}
	return nil
}

func (a *Agent) persistHookFinished(ctx context.Context, invocation hookInvocation, out hooks.Output) error {
	if invocation.HookID == "" || invocation.EffectID == "" || invocation.Stage == "" {
		return errors.New("agent: hook journal finish identity is required")
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("agent: encode hook output: %w", err)
	}
	if len(data) > maxHookFactOutputBytes {
		return fmt.Errorf("agent: hook output exceeds %d bytes", maxHookFactOutputBytes)
	}
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	// Hook output is a completion fact. It remains recordable after the hook's
	// context is cancelled; turning a known outcome into a persistence failure
	// would make ordinary user interruption indistinguishable from corruption.
	if ctx == nil {
		ctx = context.Background()
	}
	ref, err := p.putBlob(context.Background(), data, "application/json")
	if err != nil {
		return fmt.Errorf("agent: persist hook output: %w", err)
	}
	status := "success"
	if ctx.Err() != nil {
		status = "cancelled"
	} else if out.Decision == hooks.DecisionDeny || out.Decision == hooks.DecisionBlock {
		status = "denied"
	}
	event := session.HookFinished{HookID: invocation.HookID, EffectID: invocation.EffectID, Stage: invocation.Stage, Status: status, Output: ref}
	if status == "denied" {
		event.Error = out.Reason
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	if _, err := p.Commit(session.Batch{
		TransactionID: stableID("hook-finished", invocation),
		Events:        []session.Event{event},
	}); err != nil {
		return fmt.Errorf("agent: persist HookFinished: %w", err)
	}
	return nil
}
