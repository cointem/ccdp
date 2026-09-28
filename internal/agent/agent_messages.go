package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/protocol"
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
		r.fact.PendingMessages = r.fact.PendingMessages[:len(r.fact.PendingMessages)-1]
		return err
	}
	return nil
}

func (s *SessionSupervisor) flushChildMessages(r *managedRun, child *Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.fact.PendingMessages) > 0 {
		input := r.fact.PendingMessages[0]
		cmd := protocol.NewSubmitInput(protocol.CommandID(input.ID), protocol.SessionID(child.sessionID), input.ID, input.Text, protocol.InputMessage)
		receipt := child.applySubmitInput(cmd)
		if receipt.Rejected() {
			return fmt.Errorf("child mailbox admission: %v", receipt.Error)
		}
		pending := r.fact.PendingMessages
		r.fact.PendingMessages = pending[1:]
		if err := s.persist(r, "mail-delivered-"+string(input.ID)); err != nil {
			r.fact.PendingMessages = pending
			return err
		}
	}
	return nil
}

// Reserve workspace-changing workflows against queued/running shared workers.
// Child admission uses the same supervisor lock. No file ownership service is
// introduced: this protects harness workflows, not arbitrary concurrent shells.
func (a *Agent) reserveWorkspaceMutation() (func(), error) {
	s := a.supervisor
	if s == nil {
		return func() {}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspaceMutation {
		return nil, errors.New("another workspace operation is running")
	}
	for _, r := range s.children {
		r.mu.Lock()
		shared := r.fact.Child.WorkspaceMode == "shared" && r.fact.Child.Role == "worker"
		done := r.done
		r.mu.Unlock()
		if shared {
			select {
			case <-done:
			default:
				return nil, errors.New("stop or wait for shared worker agents before changing workspace state")
			}
		}
	}
	s.workspaceMutation = true
	return func() { s.mu.Lock(); s.workspaceMutation = false; s.mu.Unlock() }, nil
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
func (a *Agent) hasTurnStartingInputLocked() bool { return hasTurnStartingInput(a.pendingInputs) }

func (a *Agent) hasAgentMessage() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, input := range a.pendingInputs {
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
	if a.supervisor == nil {
		return errors.New("agent messaging unavailable")
	}
	sender := a.sessionID
	body := protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "message", AgentID: protocol.SessionID(sender), Text: text})
	var recipient *Agent
	if a.childState != nil {
		if a.childState.purpose != childPurposeTask || target != "parent" {
			return errors.New("children may message only parent")
		}
		r, err := a.supervisor.lookup(protocol.SessionID(a.sessionID))
		if err != nil {
			return err
		}
		recipient = r.parent
	} else {
		a.supervisor.mu.Lock()
		defer a.supervisor.mu.Unlock()
		r := a.supervisor.children[protocol.SessionID(target)]
		if r == nil {
			return errors.New("unknown child session")
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.parent != a || r.fact.Child.Purpose != childPurposeTask {
			return errors.New("message target is not your task child")
		}
		recipient = r.agent
		if recipient == nil || r.fact.Child.Run.Status != "running" {
			return a.supervisor.queueDormantMessage(r, id, body)
		}
	}
	cmd := protocol.NewSubmitInput(id, protocol.SessionID(recipient.sessionID), protocol.InputID(id), body, protocol.InputMessage)
	// This admission method owns the durable inbox lock and does not start a
	// model for InputMessage. Avoid waiting on the receiver's control loop.
	receipt := recipient.applySubmitInput(cmd)
	if receipt.Rejected() {
		return fmt.Errorf("message rejected: %v", receipt.Error)
	}
	return nil
}
