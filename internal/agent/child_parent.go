package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/mcp"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
	"ccdp/internal/supervision"
	"ccdp/internal/tools"
	"context"
)

// Application capabilities needed by child preparation and result delivery.
// The independent directory only knows run handles; this adapter never exposes
// parent fields, locks or the parent's writable journal to a child runtime.
type childParent interface {
	historyArtifactSource
	SessionID() string
	AdmitMessage(protocol.Command) (protocol.Receipt, error)
	childParentClosed() bool
	childPolicy() *sandbox.Sandbox
	childResultCapture() supervision.ResultCapture
	childResultBudget() int
	childSlotsForRun() *childSlots
	prepareChild(string, string) (config.Config, Options, *mcp.StepLease, *tools.Lease, []plugin.PreToolHook, error)
	runChildHook(context.Context, bool, string)
	runReviewWith(context.Context, protocol.Command, reviewExecutor) (string, protocol.ExecutionOutcome, error)
	enqueueChildRecord(supervision.RunRecord) <-chan error
	waitChildRecord(<-chan error) error
	markPersistenceFailure(error)
	signalChildActivity()
	publishState()
}

// The root directory observer and cleanup owner has no preparation or journal
// write capability. Descendant usage crosses its dedicated settlement message.
type supervisorRoot interface {
	protocol.SessionClient
	usageAccountTarget
	SessionID() string
	ReadRunRecords(context.Context) ([]session.Record, journal.ArtifactReader, error)
	WaitChildren(context.Context) error
	rootContext() context.Context
	signalChildActivity()
	publishChildUpdate(protocol.Update)
	removeChildWorkspace(context.Context, string) error
	removeMergedWorkspace(context.Context, protocol.ChildSession) error
	emitStatus(string, ...any)
}

func (a *Agent) childResultBudget() int { return a.configSnapshot().MaxResultSizeChars }

func (a *Agent) rootContext() context.Context { return a.lifecycle.rootCtx }
func (a *Agent) childParentClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lifecycle.closing || a.lifecycle.closed
}
func (a *Agent) childPolicy() *sandbox.Sandbox {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services.sandbox.Snapshot()
}
func (a *Agent) childResultCapture() supervision.ResultCapture {
	return workspaceResultCapture{store: a.codingState().store}
}
func (a *Agent) childSlotsForRun() *childSlots { return slotsForParent(a) }
func (a *Agent) signalChildActivity()          { a.mu.Lock(); a.signalAgentActivityLocked(); a.mu.Unlock() }
func (a *Agent) publishChildUpdate(update protocol.Update) {
	a.mu.Lock()
	a.enqueueViewsLocked(update)
	a.mu.Unlock()
}
func (a *Agent) runChildHook(ctx context.Context, start bool, text string) {
	if start {
		a.runHookWithJournal(ctx, hooks.EventSubagentStart, func(ctx context.Context) hooks.Output { return a.services.hooks.SubagentStart(ctx, text) })
	} else {
		a.runHookWithJournal(ctx, hooks.EventSubagentStop, func(ctx context.Context) hooks.Output { return a.services.hooks.SubagentStop(ctx, text) })
	}
}
func (a *Agent) historyArtifacts() (journal.ArtifactReader, error) {
	p := a.persistenceHandle()
	if p == nil {
		return nil, session.ErrClosed
	}
	return p.RequestArtifacts()
}
