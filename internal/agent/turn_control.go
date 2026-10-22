package agent

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

// historyLen returns the number of messages in the conversation.
func (a *Agent) historyLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.historyLocked())
}
