package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ccdp/internal/events"
	"ccdp/internal/hooks"
)

func (a *Agent) ensureRun() {
	a.mu.Lock()
	started := a.lifecycle.runStartedFlag
	closed := a.lifecycle.closed || a.lifecycle.closing
	a.mu.Unlock()
	if !started && !closed {
		go a.Run()
	}
}

// Context is the root cancellation context owned by the runtime. It is
// read-only to consumers; cancellation is performed by Interrupt/Close.
func (a *Agent) Context() context.Context { return a.lifecycle.rootCtx }

// Done closes when Close has completed all runtime-owned work and resources.
func (a *Agent) Done() <-chan struct{} { return a.lifecycle.closeDone }

// Close is idempotent and joins the runtime before closing providers and
// session resources. The external Event channel is deliberately not closed:
// its caller owns that channel and may use it for more than one adapter.
func (a *Agent) Close() { _ = a.CloseContext(context.Background()) }

// CloseContext cancels all turn/approval work, waits for it to stop, and then
// releases session resources. A context deadline reports a cleanup error while
// preserving the idempotent close state; a second CloseContext observes the
// same result.
func (a *Agent) CloseContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.lifecycle.closeOnce.Do(func() { go a.closeAsync() })
	select {
	case <-a.lifecycle.closeDone:
		return a.lifecycle.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) closeAsync() {
	// Root cancellation is concurrency-safe and independent of the owner.
	// It reaches blocked provider/tool work even while a durable commit waits.
	if a.lifecycle.rootCancel != nil {
		a.lifecycle.rootCancel()
	}
	go a.Run() // also covers Close before the first Run/Submit
	started := make(chan struct{})
	a.owner.control <- closeStarted{done: started}
	<-started

	a.pauseGoalFromWorker("session closed; resume the goal to continue")
	if a.services.supervisorOwner && a.services.supervisor != nil {
		a.services.supervisor.close()
	}
	// Drain any synchronous command already in progress before releasing its
	// resources. Keep the loop alive during cleanup and child outbox settlement.
	ready := make(chan struct{})
	a.owner.control <- finalizeBarrier{ready: ready}
	<-ready
	a.lifecycle.turnWG.Wait()
	a.lifecycle.operationWG.Wait()
	// Every worker enqueues completion before releasing its reservation. A
	// second FIFO barrier drains those messages, including terminal publication,
	// while closing prevents them from admitting replacement work.
	settled := make(chan struct{})
	a.owner.control <- finalizeBarrier{ready: settled}
	<-settled
	a.finishClose()
	close(a.owner.stop)
	<-a.owner.done
	close(a.lifecycle.closeDone)
}

func (a *Agent) finishClose() {
	var closeErr error
	// rootCtx is canceled before this method runs, so lifecycle hooks must use
	// an independent, bounded cleanup context.  A hung hook must not turn Close
	// into an unbounded wait or make the later owner releases unreachable.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	if a.catalogNotificationsDone != nil {
		<-a.catalogNotificationsDone
	}
	a.services.sessionReg.RunDispose(a.conversation.sessionID)
	a.services.evbus.Emit(events.TopicSessionDisposed, events.SessionEvent{ID: a.conversation.sessionID})
	if hookOut := a.runHookWithJournal(cleanupCtx, hooks.EventSessionEnd, func(hookCtx context.Context) hooks.Output {
		return a.services.hooks.SessionEnd(hookCtx)
	}); hookOut.Decision == hooks.DecisionDeny || hookOut.Decision == hooks.DecisionBlock {
		if hookOut.Reason != "" {
			closeErr = errors.Join(closeErr, fmt.Errorf("session end hook: %s", hookOut.Reason))
		}
	}
	if hookOut := a.runHookWithJournal(cleanupCtx, hooks.EventStop, func(hookCtx context.Context) hooks.Output {
		return a.services.hooks.Stop(hookCtx)
	}); hookOut.Decision == hooks.DecisionDeny || hookOut.Decision == hooks.DecisionBlock {
		if hookOut.Reason != "" {
			closeErr = errors.Join(closeErr, fmt.Errorf("stop hook: %s", hookOut.Reason))
		}
	}
	if a.services.mcp != nil {
		if err := a.services.mcp.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("MCP close: %w", err))
		}
	}
	if a.services.resources != nil {
		if err := a.services.resources.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("resources close: %w", err))
		}
	}
	outcome := "success"
	if closeErr != nil {
		outcome = "error"
	}
	closedFact := make(chan error, 1)
	a.owner.control <- sessionClosingPrepared{outcome: outcome, result: closedFact}
	if err := <-closedFact; err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("session close fact: %w", err))
	}
	closeErr = errors.Join(closeErr, a.finalizeChildOutbox(closeErr))
	writerClosed := make(chan error, 1)
	a.owner.control <- writerCloseRequested{result: writerClosed}
	if err := <-writerClosed; err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("persistence close: %w", err))
	}
	a.closeTrace()
	if a.services.host != nil {
		if err := a.services.host.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("extension host close: %w", err))
		}
	}
	if err := cleanupCtx.Err(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("cleanup deadline: %w", err))
	}

	finished := make(chan struct{})
	a.owner.control <- closeFinished{err: closeErr, done: finished}
	<-finished
	a.closeWatchers()
}
