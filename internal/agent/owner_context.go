package agent

import (
	"context"
	"errors"
	"fmt"

	"ccdp/internal/messages"
	"ccdp/internal/session"
)

type historyReplacementPrepared struct {
	ctx     context.Context
	history []messages.Message
	source  string
	turnID  string
	compact bool
	result  chan error
}

func (historyReplacementPrepared) ownerControlMessage() {}

func historySource(history []messages.Message) string {
	return messages.StableID("history-source", history)
}

func (a *Agent) replaceHistoryFromWorker(request historyReplacementPrepared) error {
	request.history = messages.Clone(request.history)
	if a.owner == nil {
		return a.installHistoryReplacement(request)
	}
	request.result = make(chan error, 1)
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
func (a *Agent) installHistoryReplacement(request historyReplacementPrepared) error {
	if request.ctx != nil {
		if err := request.ctx.Err(); err != nil {
			return err
		}
	}
	a.mu.Lock()
	stale := request.source != "" && historySource(a.historyLocked()) != request.source
	turnID := request.turnID
	if turnID == "" {
		turnID = fmt.Sprintf("turn-%d", a.sequences.turnSeq)
	}
	a.mu.Unlock()
	if stale {
		return errors.New("conversation changed while compaction was being prepared")
	}
	p := a.persistenceHandle()
	if p == nil {
		return a.toolJournalFailure(session.ErrClosed)
	}
	if err := p.PersistHistoryMessages(request.history, turnID); err != nil {
		return a.toolJournalFailure(err)
	}
	a.mu.Lock()
	a.conversation.recoveryHistory = nil
	a.turnMemory.tokenBaseline.promptTokens, a.turnMemory.tokenBaseline.historyLen = 0, 0
	if request.compact {
		a.lifecycle.compactFailure = nil
	}
	a.mu.Unlock()
	return nil
}
