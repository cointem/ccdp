package journal

import (
	"context"

	"ccdp/internal/messages"
	"ccdp/internal/session"
)

type CommandState struct {
	Scheduled      *session.CommandScheduled
	Completed      *session.CommandCompleted
	DigestMismatch bool
}

func (p *Writer) CommandState(id, digest string) (CommandState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	projection, err := p.projectionLocked()
	if err != nil {
		return CommandState{}, err
	}
	state := CommandState{}
	if prior, exists := projection.CommandDigests[id]; exists {
		state.DigestMismatch = prior != digest
	}
	if event, ok := projection.ScheduledCommands[id]; ok {
		copy := event
		state.Scheduled = &copy
		state.DigestMismatch = event.InputDigest != digest
		return state, nil
	}
	if event, ok := projection.CompletedCommands[id]; ok {
		copy := event
		state.Completed = &copy
	}
	return state, nil
}

func (p *Writer) CompleteCommandWithOutput(event session.CommandCompleted, output string) error {
	var err error
	p.mu.Lock()
	if p.store == nil {
		err = session.ErrClosed
	} else {
		if output != "" {
			ref, blobErr := p.putBlobLocked(context.Background(), []byte(output), "text/plain; charset=utf-8")
			if blobErr != nil {
				err = blobErr
			} else {
				event.Output = ref
			}
		}
		if err == nil {
			_, err = p.commitLocked(session.Batch{TransactionID: messages.StableID("command-completed", event), Events: []session.Event{event}})
		}
	}
	p.mu.Unlock()
	return err
}
