package agent

import (
	"fmt"
)

// interrupted reports whether the current turn was interrupted or stopped.
func (a *Agent) interrupted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turn.interruptFlag
}

// interrupt cancels preparation, hooks, model calls and tools for this turn.
func (a *Agent) interrupt() {
	a.cancelWork("interrupted by user")
}

func (a *Agent) cancelWork(reason string) {
	a.pauseGoal(reason)
	a.mu.Lock()
	a.turn.interruptFlag = true
	cancel := a.turn.turnCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if a.services.supervisor != nil {
		a.services.supervisor.cancelDelegated(a)
	}
}

// clearHistory wipes the conversation (keeps the system message).
func (a *Agent) clearHistory() {
	if a.isBusy() {
		a.emitStatus("cannot clear while a turn is running")
		return
	}
	if err := a.clearHistoryCommand(fmt.Sprintf("turn-%d", a.currentTurnSeq())); err != nil {
		a.emitStatus("failed to clear history: %v", err)
	}
}

// removeLast drops the trailing n messages from the conversation.
func (a *Agent) removeLast(n int) {
	if a.isBusy() {
		a.emitStatus("cannot remove messages while a turn is running")
		return
	}
	if err := a.removeMessagesCommand(n, fmt.Sprintf("turn-%d", a.currentTurnSeq())); err != nil {
		a.emitStatus("failed to remove messages: %v", err)
	}
}

// rewindTo keeps only the first n messages, dropping everything after them.
func (a *Agent) rewindTo(n int) {
	if a.isBusy() {
		a.emitStatus("cannot rewind while a turn is running")
		return
	}
	if err := a.rewindMessagesCommand(n, fmt.Sprintf("turn-%d", a.currentTurnSeq())); err != nil {
		a.emitStatus("failed to rewind history: %v", err)
	}
}

// historyLen returns the number of messages in the conversation.
func (a *Agent) historyLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.historyLocked())
}
