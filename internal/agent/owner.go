package agent

import (
	"ccdp/internal/protocol"
	"sync"
)

// sessionOwner serializes command admission and turn handoff. Its control lane
// outlives execution cancellation so shutdown can drain reliable completions.
// Agent.mu protects compatibility readers and producer mailboxes; business
// transitions and durable projection updates execute in this loop.
type sessionOwner struct {
	decisions           []*decisionRequest
	activeDecision      *decisionRequest
	commands            chan runtimeCommand
	control             chan ownerControl
	stop                chan struct{}
	started             chan struct{}
	done                chan struct{}
	once                sync.Once
	deferred            []runtimeCommand              // ordered admission while a settings candidate is prepared
	barriers            []finalizeBarrier             // released only after the candidate settles
	operations          []operationCompleted          // inbox append/take protected by Agent.mu
	childRecords        []childRecordRequest          // reliable FIFO; append/take protected by Agent.mu
	cancellationSignals map[protocol.CommandID]uint64 // producer signal identity, protected by Agent.mu
	rejections          []rejectedCommand             // producer mailbox, protected by Agent.mu
	childAccounted      map[string]bool               // durable terminal markers, owner access only
}

func newSessionOwner() *sessionOwner {
	return &sessionOwner{
		commands: make(chan runtimeCommand, 64),
		control:  make(chan ownerControl, 16),
		stop:     make(chan struct{}),
		started:  make(chan struct{}),
		done:     make(chan struct{}),
	}
}
