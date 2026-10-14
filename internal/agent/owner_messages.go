package agent

import (
	"context"
	"errors"

	"ccdp/internal/protocol"
)

// ownerControl is a reliable lifecycle lane, independent of the execution
// context. Cancelling provider/tool work cannot discard finalization messages.
type ownerControl interface{ ownerControlMessage() }

// finalizeBarrier proves all earlier synchronous command handling has returned.
// Resource cleanup runs outside the loop, which remains available for results.
type finalizeBarrier struct{ ready chan struct{} }

func (finalizeBarrier) ownerControlMessage() {}

// turnCompleted transfers terminal publication and queued-input handoff to the
// owner. The sequence prevents a delayed completion from settling another turn.
type turnCompleted struct {
	sequence uint64
	saveErr  error
}

func (turnCompleted) ownerControlMessage() {}

// operationCompleted owns values returned by a command worker. Cancellation
// affects execution; it never removes this completion from the owner's inbox.
type operationCompleted struct {
	ctx          context.Context
	cancel       context.CancelFunc
	command      protocol.Command
	name, output string
	err          error
	exclusive    bool
}

type completionsReady struct{}

func (completionsReady) ownerControlMessage() {}

func (a *Agent) postOperationCompleted(result operationCompleted) {
	a.mu.Lock()
	a.owner.operations = append(a.owner.operations, result)
	a.mu.Unlock()
	a.ensureRun()
	select {
	case a.owner.control <- completionsReady{}:
	default:
	}
}

func (a *Agent) drainOperationCompletions() {
	a.mu.Lock()
	pending := a.owner.operations
	a.owner.operations = nil
	a.mu.Unlock()
	for _, result := range pending {
		a.settleOperation(result)
	}
}

// sessionClosingPrepared records resource cleanup before the child outbox.
// The worker retains the writer until the owner's acknowledgement.
type sessionClosingPrepared struct {
	outcome string
	result  chan error
}

func (sessionClosingPrepared) ownerControlMessage() {}

func (a *Agent) commitSessionClosing(outcome string) error {
	a.mu.Lock()
	pending := a.finalization
	a.mu.Unlock()
	var err error
	if pending != nil {
		err = a.cancelRunInputs(string(pending.request.Fact.Child.Run.ID))
	}
	if err != nil {
		outcome = "error"
	}
	return errors.Join(err, a.persistSessionClosed(outcome))
}
