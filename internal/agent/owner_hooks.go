package agent

import (
	"fmt"

	"ccdp/internal/messages"
	"ccdp/internal/session"
)

type hookStartedRequested struct {
	invocation hookInvocation
	result     chan error
}

func (hookStartedRequested) ownerControlMessage() {}

type hookFinishedPrepared struct {
	invocation hookInvocation
	fact       session.HookFinished
	result     chan error
}

func (hookFinishedPrepared) ownerControlMessage() {}

func (a *Agent) admitHook(invocation hookInvocation) error {
	if !a.constructionComplete {
		return a.commitHookStart(invocation)
	}
	a.ensureRun()
	request := hookStartedRequested{invocation: invocation, result: make(chan error, 1)}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}
func (a *Agent) commitHookStart(invocation hookInvocation) error {
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	p := a.persistenceHandle()
	if p == nil {
		return session.ErrClosed
	}
	if _, err := p.Commit(session.Batch{TransactionID: messages.StableID("hook-started", invocation), Events: []session.Event{session.HookStarted{HookID: invocation.HookID, EffectID: invocation.EffectID, Stage: invocation.Stage}}}); err != nil {
		return fmt.Errorf("agent: persist HookStarted: %w", err)
	}
	return nil
}
func (a *Agent) settleHook(invocation hookInvocation, fact session.HookFinished) error {
	request := hookFinishedPrepared{invocation: invocation, fact: fact, result: make(chan error, 1)}
	if !a.constructionComplete {
		return a.commitHookResult(request)
	}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}
func (a *Agent) commitHookResult(request hookFinishedPrepared) error {
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	p := a.persistenceHandle()
	if p == nil {
		return session.ErrClosed
	}
	if _, err := p.Commit(session.Batch{TransactionID: messages.StableID("hook-finished", request.invocation), Events: []session.Event{request.fact}}); err != nil {
		return fmt.Errorf("agent: persist HookFinished: %w", err)
	}
	return nil
}
