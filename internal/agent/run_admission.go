package agent

// beginRuntimeCommand reserves a live run while its directory lock is released
// for a cross-owner call. The driver cannot settle an idle view until every
// admitted command has returned; completing the last call wakes that check.
// Caller holds r.mu.
func (r *managedRun) beginRuntimeCommand() {
	r.commandsInFlight++
	if r.commandChanges == nil {
		r.commandChanges = make(chan struct{}, 1)
	}
}
func (r *managedRun) finishRuntimeCommand() {
	r.mu.Lock()
	r.commandsInFlight--
	if r.commandsInFlight == 0 {
		select {
		case r.commandChanges <- struct{}{}:
		default:
		}
	}
	r.mu.Unlock()
}
func (b childRunBoundary) Changes() <-chan struct{} {
	b.run.mu.Lock()
	defer b.run.mu.Unlock()
	if b.run.commandChanges == nil {
		b.run.commandChanges = make(chan struct{}, 1)
	}
	return b.run.commandChanges
}
