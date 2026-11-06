package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/sessionview"
	"ccdp/internal/supervision"
)

type managedClient struct {
	supervisor *SessionSupervisor
	run        *managedRun
	id         protocol.SessionID
	initialRun protocol.RunID
}

func (c *managedClient) current() *managedRun {
	r, err := c.supervisor.lookup(c.id)
	if err == nil {
		return r
	}
	return c.run
}

func (c *managedClient) Snapshot(ctx context.Context) (protocol.SessionView, error) {
	if err := ctx.Err(); err != nil {
		return protocol.SessionView{}, err
	}
	r := c.current()
	r.mu.Lock()
	if r.agent != nil {
		view, err := r.agent.Snapshot(ctx)
		view.RunID = r.fact.Child.Run.ID
		if r.fact.Child.Run.Status == "settling" {
			view.Phase = protocol.PhaseFinalizing
			view.Closing = false
		}
		r.mu.Unlock()
		return view, err
	}
	view := protocol.CloneSessionView(r.final)
	row := r.fact.Child
	r.mu.Unlock()
	if view.SessionID != "" {
		view.RunID = row.Run.ID
		// A closed runtime is still a readable session. Preserve its settings,
		// but expose the settled run as idle and load the bounded transcript.
		view.Busy, view.Closing, view.Phase = false, false, protocol.PhaseIdle
		view.Approval, view.Question = nil, nil
		if view.Transcript == nil {
			page, err := c.supervisor.ReadTranscript(ctx, row.SessionID, 0, sessionview.Window)
			if err != nil {
				return view, err
			}
			view.Transcript, view.TranscriptMore = page.Items, page.More
		}
		data, _ := json.Marshal(view)
		var clone protocol.SessionView
		_ = json.Unmarshal(data, &clone)
		return clone, nil
	}
	view = protocol.SessionView{SessionID: row.SessionID, RunID: row.Run.ID, Busy: row.Run.Active(), Phase: protocol.PhaseIdle, Usage: row.Run.Usage}
	view.Settings.Workspace = row.Workspace
	if row.Run.Active() {
		view.Phase = protocol.PhasePreparing
	}
	if row.Approval != nil {
		view.Approval, view.Phase = row.Approval, protocol.PhaseWaitingApproval
	}
	page, err := c.supervisor.ReadTranscript(ctx, row.SessionID, 0, sessionview.Window)
	if err != nil && !errors.Is(err, os.ErrNotExist) && row.Run.Status != "queued" && row.Run.Status != "starting" {
		return view, err
	}
	view.Transcript, view.TranscriptMore = page.Items, page.More
	if row.Purpose == childPurposeReview && !row.Run.Active() && len(view.Transcript) == 0 {
		text := row.Run.Output
		if text == "" {
			text = row.Run.Error
		}
		if text != "" {
			view.Transcript = []protocol.TranscriptItem{{ID: "review-report-" + string(row.Run.ID), Kind: "assistant", Text: text, Status: "completed"}}
		}
	}
	// Recover presentation metadata without starting a runtime/provider.
	records, _, readErr := c.supervisor.readSessionRecords(ctx, row.SessionID)
	if readErr == nil {
		for _, record := range records {
			if usage, ok := record.Event.(*session.UsageChanged); ok {
				view.Usage.Cache = usage.Usage.Cache
			}
			if changed, ok := record.Event.(*session.SettingsChanged); ok {
				s := changed.Settings
				window := s.EffectiveContextWindow
				if window == 0 {
					window = s.ContextWindow
				}
				maxOutputTokens := 0
				if s.MaxOutputTokens != nil {
					maxOutputTokens = *s.MaxOutputTokens
				}
				view.Settings = protocol.SettingsSnapshot{
					Workspace: s.Workspace,
					Revision:  changed.Revision,
					Model:     protocol.ModelBinding{Model: s.Model, Provider: s.Provider, Endpoint: s.Endpoint},

					Permission: protocol.PermissionPolicy{Mode: s.PermissionPolicy, AlwaysAllow: s.AlwaysAllow, AlwaysDeny: s.AlwaysDeny, Revision: changed.Revision},
					Sandbox: protocol.SandboxPolicy{NetworkAccess: s.NetworkAccess, LocalDevelopmentNetwork: s.LocalDevelopmentNetwork,
						AdditionalDirectories:         s.AdditionalDirectories,
						AdditionalReadOnlyDirectories: s.AdditionalReadOnlyDirectories,
						DisallowedDirectories:         s.DisallowedDirectories, Revision: changed.Revision},
					ReasoningEffort: s.ReasoningEffort, Verbosity: s.Verbosity,
					ContextWindow: window, CompactThreshold: s.CompactThreshold,
					MaxOutputTokens: maxOutputTokens, MaxTurns: s.MaxTurns,
				}
			}
			view.Revision.LogSeq = uint64(record.Seq)
		}
	}
	return view, nil
}

func (c *managedClient) Watch(ctx context.Context, cursor protocol.Cursor) (protocol.Subscription, error) {
	return supervision.Watch(ctx, c.supervisor.root.rootContext(), c)
}

func (c *managedClient) ReadOperation(ctx context.Context, id protocol.CommandID) (protocol.OperationResult, error) {
	records, blobs, err := c.supervisor.readSessionRecords(ctx, c.id)
	if err != nil {
		return protocol.OperationResult{ID: id}, err
	}
	return readOperationResult(ctx, records, blobs, id)
}

func (c *managedClient) ReadInput(ctx context.Context, id protocol.InputID) (protocol.InputResult, error) {
	records, _, err := c.supervisor.readSessionRecords(ctx, c.id)
	if err != nil {
		return protocol.InputResult{ID: id}, err
	}
	return readInputResult(ctx, records, id)
}

func (c *managedClient) AttachedRun() supervision.ObservedRun {
	r := c.current()
	r.mu.Lock()
	defer r.mu.Unlock()
	row := supervision.ObservedRun{ID: r.fact.Child.Run.ID, Status: r.fact.Child.Run.Status}
	if r.agent != nil {
		row.Runtime = r.agent
	}
	return row
}

func (c *managedClient) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	cmd, err := cmd.Normalize()
	if err != nil {
		return protocol.Receipt{}, err
	}
	if cmd.SessionID != c.id {
		return protocol.Receipt{}, errors.New("command target differs from viewed session")
	}
	r := c.current()
	if cmd.Type == protocol.CommandSubmitInput {
		// A continuation command is also durable before its InputQueued fact.
		// Preserve that identity and digest check even if a later run replaced it.
		priorContinuation := c.supervisor.directory.Run(protocol.RunID("continue-" + string(cmd.ID)))
		if priorContinuation != nil {
			// Check the continuation identity before returning its input receipt.
			// A different ExpectedRunID must not reuse the command identity.
			if _, err := c.supervisor.Control(ctx, protocol.AgentControl{ID: cmd.ID, SessionID: cmd.SessionID, RunID: cmd.ExpectedRunID, Action: "continue", Text: cmd.Input.Text, Input: cmd.Input}); err != nil {
				return protocol.Receipt{}, err
			}
		}
		receipt, found, err := c.supervisor.priorChildInputReceipt(ctx, r, cmd)
		if err != nil || found {
			return receipt, err
		}
		r.mu.Lock()
		settled := !r.fact.Child.Run.Active()
		r.mu.Unlock()
		if settled || priorContinuation != nil {
			_, err := c.supervisor.Control(ctx, protocol.AgentControl{ID: cmd.ID, SessionID: cmd.SessionID, RunID: cmd.ExpectedRunID, Action: "continue", Text: cmd.Input.Text, Input: cmd.Input})
			if err != nil {
				return protocol.Receipt{}, err
			}
			return protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: protocol.ReceiptScheduled}, nil
		}
	}
	r.mu.Lock()
	locked := true
	defer func() {
		if locked {
			r.mu.Unlock()
		}
	}()
	row := r.fact.Child
	if cmd.SessionID != row.SessionID {
		return protocol.Receipt{}, errors.New("command target differs from viewed session")
	}
	expected := cmd.ExpectedRunID
	if expected == "" {
		expected = c.initialRun
	}
	if expected != row.Run.ID {
		return protocol.Receipt{}, errors.New("stale run id")
	}
	if !row.Run.Active() || row.Run.Status == "settling" {
		return protocol.Receipt{}, errors.New("run settled; use continue")
	}
	switch cmd.Type {
	case protocol.CommandSubmitInput:
		if row.Purpose != childPurposeTask && row.Purpose != childPurposeReview {
			return protocol.Receipt{}, errors.New("guardian is read-only")
		}
	case protocol.CommandApproveTool:
		if r.opts.NonInteractive {
			return protocol.Receipt{}, errors.New("approval channel unavailable")
		}
	case protocol.CommandInterrupt, protocol.CommandAnswerQuestion:
	case protocol.CommandStop:
		r.cancel()
		return protocol.Receipt{CommandID: cmd.ID, SessionID: row.SessionID, Status: protocol.ReceiptApplied}, nil
	default:
		return protocol.Receipt{}, fmt.Errorf("%s is unavailable in a child view; press Esc to return to the main agent", cmd.Type)
	}
	if r.agent == nil {
		return protocol.Receipt{}, errors.New("child has not started")
	}
	// Preserve the runtime's input identity, scheduled/applied state, revision
	// and rejection. Never manufacture a successful receipt for a queued input.
	child := r.agent
	r.beginRuntimeCommand()
	r.mu.Unlock()
	locked = false
	defer r.finishRuntimeCommand()
	return child.Submit(ctx, cmd)
}

// priorChildInputReceipt runs before continuation admission, so a duplicate
// cannot create a run which has no new input to execute.
func (s *SessionSupervisor) priorChildInputReceipt(ctx context.Context, r *managedRun, cmd protocol.Command) (protocol.Receipt, bool, error) {
	r.mu.Lock()
	stopped, done := !r.fact.Child.Run.Active(), r.done
	r.mu.Unlock()
	if stopped {
		select {
		case <-done:
		case <-ctx.Done():
			return protocol.Receipt{}, false, ctx.Err()
		}
	}
	records, _, err := s.readSessionRecords(ctx, cmd.SessionID)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.Receipt{}, false, nil // queued child not constructed yet
	}
	if err != nil {
		return protocol.Receipt{}, false, err
	}
	body := *cmd.Input
	if body.ID == "" {
		body.ID = protocol.InputID(cmd.ID)
	}
	input := durableInputs(records)[body.ID]
	if input == nil {
		return protocol.Receipt{}, false, nil
	}
	digest, err := submitInputDigest(body)
	if err != nil {
		return protocol.Receipt{}, false, err
	}
	priorDigest, err := input.inputDigest()
	if err != nil {
		return protocol.Receipt{}, false, err
	}
	receipt := input.receipt(cmd.ID, cmd.SessionID)
	if len(records) > 0 {
		receipt.Revision.LogSeq = records[len(records)-1].Seq
	}
	if digest != priorDigest {
		receipt.Status = protocol.ReceiptRejected
		receipt.Error = &protocol.CommandError{Code: protocol.ErrorInvalidCommand, Message: "input id was already used for different text, strategy or images"}
	} else if stopped && receipt.Status == protocol.ReceiptScheduled && body.Strategy != protocol.InputMessage {
		receipt.Status = protocol.ReceiptRejected
		receipt.Error = &protocol.CommandError{Code: protocol.ErrorInvalidState, Message: "input was already accepted but not delivered before the run stopped; use a new input id to continue"}
	}
	return receipt, true, nil
}
