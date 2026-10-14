package agent

// A failed automatic summary suppresses only the exact state it tried. The
// worker reports the identity and diagnostic; the owner validates current
// state before installing the retry suppression.
type compactionFailed struct {
	key, reason string
	done        chan struct{}
}

func (compactionFailed) ownerControlMessage() {}

func (a *Agent) rememberCompactFailure(key string, reason error) {
	if key == "" || reason == nil {
		return
	}
	request := compactionFailed{key: key, reason: reason.Error(), done: make(chan struct{})}
	if a.owner == nil {
		a.rememberCompactFailureOwned(request)
		return
	}
	a.ensureRun()
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
func (a *Agent) rememberCompactFailureOwned(request compactionFailed) {
	a.mu.Lock()
	if a.compactFailureKeyLocked() == request.key {
		a.lifecycle.compactFailure = &compactFailureState{key: request.key, reason: request.reason}
	}
	a.mu.Unlock()
}
