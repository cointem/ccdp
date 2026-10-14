package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/supervision"
	"ccdp/internal/tools"
)

func childControlTool(name string) bool {
	switch name {
	case "SpawnAgent", "FollowupAgent", "StopAgent", "ListAgents", "ReadAgent", "WaitAgent":
		return true
	}
	return false
}

func childMessageTool(tool tools.Tool) bool {
	t, ok := tool.(*tools.AgentTool)
	return ok && t.Name() == "SendMessage"
}

// The supervisor's durable run record temporarily owns mail while no child
// runtime owns its inbox. Admission on startup transfers it using the same ID.
// Caller holds r.mu.
func (s *SessionSupervisor) queueDormantMessage(r *managedRun, id protocol.CommandID, text string) error {
	for _, input := range r.fact.PendingMessages {
		if input.ID == protocol.InputID(id) {
			if input.Text != text {
				return errors.New("message id reused")
			}
			return nil
		}
	}
	if len(r.fact.PendingMessages) >= 64 {
		return errors.New("child mailbox full; continue the child before sending more")
	}
	r.fact.PendingMessages = append(r.fact.PendingMessages, protocol.InputView{ID: protocol.InputID(id), Text: text, Strategy: protocol.InputMessage, State: "queued", CreatedAt: time.Now().UTC()})
	if err := s.persist(r, "mail-"+string(id)); err != nil {
		for i, input := range r.fact.PendingMessages {
			if input.ID == protocol.InputID(id) {
				r.fact.PendingMessages = append(r.fact.PendingMessages[:i:i], r.fact.PendingMessages[i+1:]...)
				break
			}
		}
		return err
	}
	return nil
}

func (s *SessionSupervisor) flushChildMessages(r *managedRun, child supervision.ChildRuntime) error {
	for {
		r.mu.Lock()
		if len(r.fact.PendingMessages) == 0 {
			r.mu.Unlock()
			return nil
		}
		input := r.fact.PendingMessages[0]
		r.mu.Unlock()
		cmd := protocol.NewSubmitInput(protocol.CommandID(input.ID), protocol.SessionID(child.SessionID()), input.ID, input.Text, protocol.InputMessage)
		receipt, submitErr := child.AdmitMessage(cmd)
		if submitErr != nil {
			return submitErr
		}
		if receipt.Rejected() {
			return fmt.Errorf("child mailbox admission: %v", receipt.Error)
		}
		r.mu.Lock()
		// Another delivery may already have acknowledged this stable input.
		// Never remove a newer message based on a delayed acknowledgement.
		if len(r.fact.PendingMessages) == 0 || r.fact.PendingMessages[0].ID != input.ID {
			r.mu.Unlock()
			continue
		}
		pending := r.fact.PendingMessages
		r.fact.PendingMessages = pending[1:]
		err := s.persist(r, "mail-delivered-"+string(input.ID))
		if err != nil {
			r.fact.PendingMessages = append([]protocol.InputView{input}, r.fact.PendingMessages...)
		}
		r.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// Reserve workspace-changing workflows against queued/running shared workers.
// Child admission uses the same supervisor lock. No file ownership service is
// introduced: this protects harness workflows, not arbitrary concurrent shells.
func (a *Agent) reserveWorkspaceMutation() (func(), error) {
	s := a.services.supervisor
	if s == nil {
		return func() {}, nil
	}
	return s.directory.ReserveWorkspace()
}

func hasTurnStartingInput(inputs []protocol.InputView) bool {
	for _, input := range inputs {
		if input.Strategy != protocol.InputMessage {
			return true
		}
	}
	return false
}

// Caller holds a.mu.
func (a *Agent) hasTurnStartingInputLocked() bool {
	return hasTurnStartingInput(a.conversation.durable.PendingInputViews())
}

func (a *Agent) hasAgentMessage() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, input := range a.conversation.durable.PendingInputViews() {
		if input.Strategy == protocol.InputMessage {
			return true
		}
	}
	return false
}

func (a *Agent) consumeAgentMessages() error {
	for {
		_, ok, err := a.claimPendingInputAndAppend(fmt.Sprintf("turn-%d", a.currentTurnSeq()), protocol.InputMessage)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		// InputQueued already projected a collaboration cell. Consuming it must
		// not echo it as another user message.
	}
}

// The source identity and authority come from the calling runtime, never the
// message body. Only a parent/child edge is an addressable conversation.
func (a *Agent) sendAgentMessage(ctx context.Context, id protocol.CommandID, target, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" || len(text) > 32<<10 {
		return errors.New("message must contain 1..32768 bytes")
	}
	if a.services.supervisor == nil {
		return errors.New("agent messaging unavailable")
	}
	sender := a.conversation.sessionID
	body := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "message", AgentID: protocol.SessionID(sender), Text: text})
	var recipient interface {
		SessionID() string
		AdmitMessage(protocol.Command) (protocol.Receipt, error)
	}
	if a.children.childState != nil {
		if a.children.childState.purpose != childPurposeTask || target != "parent" {
			return errors.New("children may message only parent")
		}
		r, err := a.services.supervisor.lookup(protocol.SessionID(a.conversation.sessionID))
		if err != nil {
			return err
		}
		recipient = r.parent
	} else {
		// Persist the parent's outbox before crossing into the child. No
		// directory/run lock may be held while waiting on another owner.
	retryMailbox:
		a.services.supervisor.publicationMu.Lock()
		r := a.services.supervisor.directory.Current(protocol.SessionID(target))
		if r == nil {
			a.services.supervisor.publicationMu.Unlock()
			return errors.New("unknown child session")
		}
		r.mu.Lock()
		if pending := r.handoff; pending != nil {
			r.mu.Unlock()
			a.services.supervisor.publicationMu.Unlock()
			select {
			case <-pending:
				goto retryMailbox
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if r.parent != a || r.fact.Child.Purpose != childPurposeTask {
			r.mu.Unlock()
			a.services.supervisor.publicationMu.Unlock()
			return errors.New("message target is not your task child")
		}
		a.services.supervisor.publicationMu.Unlock()
		err := a.services.supervisor.queueDormantMessage(r, id, body)
		recipient = r.agent
		child := r.agent
		running := r.fact.Child.Run.Status == "running"
		r.mu.Unlock()
		if err != nil {
			return err
		}
		if recipient != nil && running {
			// A concurrent finish may close the child's inbox. The durable
			// parent outbox retains the message for its next continuation.
			_ = a.services.supervisor.flushChildMessages(r, child)
		}
		return nil
	}
	cmd := protocol.NewSubmitInput(id, protocol.SessionID(recipient.SessionID()), protocol.InputID(id), body, protocol.InputMessage)
	// Mail uses the reliable control lane even during settings preparation.
	// InputMessage never starts a model turn.
	receipt, admitErr := recipient.AdmitMessage(cmd)
	if admitErr != nil {
		return admitErr
	}
	if receipt.Rejected() {
		return fmt.Errorf("message rejected: %v", receipt.Error)
	}
	return nil
}

// AdmitMessage is the narrow mailbox capability used during cross-session
// settlement. It cannot start a turn or accept arbitrary control commands.
func (a *Agent) AdmitMessage(cmd protocol.Command) (protocol.Receipt, error) {
	normalized, err := cmd.Normalize()
	if err != nil {
		return protocol.Receipt{}, err
	}
	if normalized.Type != protocol.CommandSubmitInput || normalized.Input == nil || normalized.Input.Strategy != protocol.InputMessage || normalized.SessionID != protocol.SessionID(a.SessionID()) {
		return protocol.Receipt{}, errors.New("mailbox admission requires a message addressed to this session")
	}
	return a.admitInputFromWorker(normalized)
}

// mailboxAdmission bypasses ordinary command deferral, so a child reporting
// during a settings drain cannot deadlock the parent waiting for that child.
type mailboxAdmission struct {
	command protocol.Command
	result  chan protocol.Receipt
}

func (mailboxAdmission) ownerControlMessage() {}

func (a *Agent) admitInputFromWorker(normalized protocol.Command) (protocol.Receipt, error) {
	if a.owner == nil {
		return a.applySubmitInput(normalized), nil
	}
	a.ensureRun()
	request := mailboxAdmission{command: normalized, result: make(chan protocol.Receipt, 1)}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return protocol.Receipt{}, session.ErrClosed
	}
	select {
	case receipt := <-request.result:
		return receipt, nil
	case <-a.owner.done:
		return protocol.Receipt{}, session.ErrClosed
	}
}
