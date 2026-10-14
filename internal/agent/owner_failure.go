package agent

// Persistence failure is a one-way concurrent signal, like context cancellation.
// Producers latch it and cancel execution immediately; only the owner installs
// the corresponding business state before acknowledging or publishing a view.
func (a *Agent) installPersistenceFailureOwned() {
	fault := a.persistenceFault.Load()
	if fault == nil {
		return
	}
	a.mu.Lock()
	if a.services.persistenceErr == nil {
		a.services.persistenceErr = *fault
	}
	if a.turn.busy {
		a.turn.interruptFlag = true
	}
	a.mu.Unlock()
}
