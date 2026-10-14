package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// commandInputDigest returns the stable identity used by CommandScheduled.
// It intentionally hashes the full normalized command body (including its
// payload), rather than a display name or operation ID.  The command body is
// never written to the session log, but the digest makes a resumed retry
// distinguish the original request from an ID collision.
func commandInputDigest(cmd protocol.Command) (string, error) {
	body, err := json.Marshal(cmd)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

// submitInputDigest uses the same canonical JSON encoding as command
// idempotency but excludes the command/session envelope. Input IDs are already
// the map key, so only the normalized input body needs to be retained for a
// live duplicate check. The submitted text therefore never remains in
// seenInputBody after admission.
func submitInputDigest(input protocol.SubmitInput) (string, error) {
	return commandInputDigest(protocol.Command{Type: protocol.CommandSubmitInput, Input: &input})
}

// persistCommandScheduled admits an asynchronous command before its worker is
// started. The digest is over the normalized protocol command, not a display
// label, so replay and Store deduplication can reject a reused ID with a
// different body without putting the command payload into the session log.
func (a *Agent) persistCommandScheduled(cmd protocol.Command, name string, sourceRevision uint64) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	digest, err := commandInputDigest(cmd)
	if err != nil {
		return fmt.Errorf("encode command admission: %w", err)
	}
	event := session.CommandScheduled{CommandID: string(cmd.ID), OperationID: string(cmd.ID), Name: name, InputDigest: digest, SourceRevision: sourceRevision}
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	_, err = p.Commit(session.Batch{TransactionID: messages.StableID("command-scheduled", event), Events: []session.Event{event}})
	if err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist command admission: %w", err))
	}
	return nil
}

// durableCommandState is read directly from the incremental raw projection.
// A scheduled command with no completion is an admitted operation whose
// external outcome is unknown; a completed command is terminal and can be
// replayed as an idempotent receipt.  The command payload itself is never
// recovered from the log.
type durableCommandState struct {
	scheduled      *session.CommandScheduled
	completed      *session.CommandCompleted
	digestMismatch bool
}

func (a *Agent) durableCommandState(cmd protocol.Command) (durableCommandState, error) {
	p := a.persistenceHandle()
	if p == nil {
		return durableCommandState{}, nil
	}
	digest, err := commandInputDigest(cmd)
	if err != nil {
		return durableCommandState{}, fmt.Errorf("encode command identity: %w", err)
	}
	return p.commandState(string(cmd.ID), digest)
}

// persistCommandCompletedWithOutput stores the bounded command output as a
// session artifact and commits its reference with the terminal fact. Blob
// publication and the event share the persistence mutex, so replay never
// observes a successful completion pointing at an unpublished output.
func (a *Agent) persistCommandCompletedWithOutput(event session.CommandCompleted, output string) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	if err := a.persistenceFailure(); err != nil {
		return err
	}
	err := p.CompleteCommandWithOutput(event, output)
	if err != nil {
		return a.toolJournalFailure(fmt.Errorf("agent: persist command completion: %w", err))
	}
	return nil
}

func (p *sessionPersistence) commandState(id, digest string) (durableCommandState, error) {
	s, err := p.Writer.CommandState(id, digest)
	return durableCommandState{scheduled: s.Scheduled, completed: s.Completed, digestMismatch: s.DigestMismatch}, err
}
