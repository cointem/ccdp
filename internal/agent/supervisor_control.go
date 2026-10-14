package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"ccdp/internal/atomicfile"
	"ccdp/internal/fsops"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

// normalizeAgentControl establishes the command identity before any lookup or
// durable admission. Retries and first admissions must hash the same body.
// Keep text-only continuations in their existing representation so persisted
// command digests remain valid across upgrades.
func normalizeAgentControl(c protocol.AgentControl) (protocol.AgentControl, error) {
	if c.ID == "" {
		return c, errors.New("command id required")
	}
	if c.Action != "continue" {
		return c, nil
	}
	if c.Input != nil {
		cmd := protocol.Command{ID: c.ID, SessionID: c.SessionID, Type: protocol.CommandSubmitInput, Input: c.Input}
		normalized, err := cmd.Normalize()
		if err != nil {
			return c, err
		}
		c.Input = normalized.Input
		c.Text = c.Input.Text
	}
	if strings.TrimSpace(c.Text) == "" && (c.Input == nil || len(c.Input.Images) == 0) {
		return c, errors.New("continue requires a message")
	}
	return c, nil
}

func (s *SessionSupervisor) Control(ctx context.Context, c protocol.AgentControl) (protocol.RunView, error) {
	var err error
	c, err = normalizeAgentControl(c)
	if err != nil {
		return protocol.RunView{}, err
	}
	if c.Action == "stop_tree" && c.SessionID == protocol.SessionID(s.root.SessionID()) {
		return protocol.RunView{Status: "cancelled"}, s.stopTree(ctx, c.ID)
	}
	if c.Action == "continue" {
		prior := s.directory.Run(protocol.RunID("continue-" + string(c.ID)))
		if prior != nil {
			prior.mu.Lock()
			defer prior.mu.Unlock()
			if prior.fact.CommandDigest != messages.StableID("continue", c) {
				return prior.fact.Child.Run, errors.New("command id reused with different continuation")
			}
			return prior.fact.Child.Run, nil
		}
	}
	r, err := s.lookup(c.SessionID)
	if err != nil {
		return protocol.RunView{}, err
	}
	if c.Action == "continue" {
		return s.continueRun(ctx, r, c)
	}
	r.mu.Lock()
	locked := true
	defer func() {
		if locked {
			r.mu.Unlock()
		}
	}()
	row := r.fact.Child
	if c.RunID != "" && c.RunID != row.Run.ID {
		return row.Run, errors.New("stale run id")
	}
	if c.Action == "cancel" {
		if row.Run.Active() {
			r.cancel()
			if row.Run.Status != "settling" {
				r.fact.Child.Run.Status = "stopping"
				row.Run.Status = "stopping"
				s.publishChild(r.fact.Child, nil)
			}
		}
		return row.Run, nil
	}
	if !row.Run.Active() || row.Run.Status == "settling" {
		return row.Run, errors.New("run settled; use continue")
	}
	if c.Action == "approve" && r.reviewApprovalReply != nil {
		if r.opts.NonInteractive || row.Approval == nil || row.Approval.ID != c.ApprovalID {
			return row.Run, errors.New("review approval is unavailable or stale")
		}
		select {
		case r.reviewApprovalReply <- c.Approve:
			return row.Run, nil
		default:
			return row.Run, errors.New("review approval already answered")
		}
	}
	if r.agent == nil {
		return row.Run, errors.New("child has not started")
	}
	cmd := protocol.Command{ID: c.ID, SessionID: row.SessionID}
	switch c.Action {
	case "send":
		if row.Purpose != childPurposeTask && row.Purpose != childPurposeReview {
			return row.Run, errors.New("guardian is read-only")
		}
		strategy := c.Strategy
		if strategy == "" {
			strategy = protocol.InputSteer
		}
		cmd = protocol.NewSubmitInput(c.ID, row.SessionID, protocol.InputID(c.ID), c.Text, strategy)
	case "interrupt":
		cmd.Type = protocol.CommandInterrupt
	case "approve":
		if r.opts.NonInteractive {
			return row.Run, errors.New("approval channel unavailable")
		}
		cmd.Type = protocol.CommandApproveTool
		cmd.Approval = &protocol.ApproveTool{ApprovalID: c.ApprovalID, Approve: c.Approve}
	default:
		return row.Run, errors.New("unsupported child control action")
	}
	child := r.agent
	r.beginRuntimeCommand()
	r.mu.Unlock()
	locked = false
	defer r.finishRuntimeCommand()
	receipt, err := child.Submit(ctx, cmd)
	if err == nil && receipt.Rejected() {
		err = fmt.Errorf("child command rejected: %v", receipt.Error)
	}
	return row.Run, err
}

func (s *SessionSupervisor) continueRun(ctx context.Context, r *managedRun, c protocol.AgentControl) (protocol.RunView, error) {
	r.mu.Lock()
	row := r.fact.Child
	system := r.fact.SystemPrompt
	commandDigest := messages.StableID("continue", c)
	if r.fact.CommandID == string(c.ID) {
		if r.fact.CommandDigest != commandDigest {
			r.mu.Unlock()
			return row.Run, errors.New("command id reused with different continuation")
		}
		r.mu.Unlock()
		return row.Run, nil
	}
	memory := append([]session.Record(nil), r.memory...)
	blobs := r.memoryBlobs
	r.mu.Unlock()
	if row.Purpose != childPurposeTask && row.Purpose != childPurposeReview {
		return row.Run, errors.New("guardian cannot continue")
	}
	if row.Run.Active() {
		return row.Run, errors.New("agent is busy; use SendMessage for information, or wait before assigning the next task")
	}
	if row.MergeStatus == "merged" {
		return row.Run, errors.New("child changes were merged and its workspace retired; start a new task")
	}
	if c.RunID != "" && c.RunID != row.Run.ID {
		return row.Run, errors.New("stale run id")
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		return row.Run, ctx.Err()
	}
	r.mu.Lock()
	if r.fact.DeliveryID != "" && !r.fact.UsageAccounted {
		if err := s.recordOutcome(r); err != nil {
			r.mu.Unlock()
			return row.Run, err
		}
	}
	r.mu.Unlock()
	cfg, opts, lease, catalog, hooks, err := s.prepare(r.parent, row.Purpose, system)
	if err != nil {
		return row.Run, err
	}
	var workspaceLease *atomicfile.Lock
	queued := false
	if row.BaselineID != "" {
		workspaceLease, err = fsops.LeaseWorkspace(row.Workspace)
		if err != nil {
			lease.Close()
			return row.Run, err
		}
		defer func() {
			if !queued {
				workspaceLease.Close()
			}
		}()
	}
	if row.Workspace != "" && row.Purpose != childPurposeReview {
		if _, statErr := os.Stat(row.Workspace); statErr != nil {
			lease.Close()
			return row.Run, fmt.Errorf("child workspace unavailable: %w", statErr)
		}
		bindChildWorkspace(&cfg, &opts, row.WorkspaceMode, row.Workspace, cfg.Workspace)
		applyChildRole(&cfg, &opts, row.Role, row.WorkspaceMode)
	}
	if row.Purpose == childPurposeReview {
		row.Workspace = ""
		cfg.Hooks, cfg.Tools, cfg.LanguageServers = nil, nil, nil
		opts.AllowedTools = map[string]bool{}
		cfg.SystemPrompt += "\nThe review copy was cleaned up. Answer only from the saved report and conversation. Do not claim to read source or rerun tests. For new source inspection ask the user to start a new /review."
	}
	opts.memoryResume, opts.memoryBlobs = memory, blobs
	var initial *SessionSnapshot
	if memory != nil {
		initial, err = snapshotFromRecords(memory, string(row.SessionID))
	} else {
		initial, err = LoadSession(cfg.SessionDir, string(row.SessionID))
	}
	if err != nil {
		lease.Close()
		return row.Run, err
	}
	// A continuation starts a new run, never replays uncertain pending tools
	// or previously accepted inputs from the interrupted run.
	initial.PendingCommands = nil
	pendingMessages := initial.PendingInputs[:0]
	for _, input := range initial.PendingInputs {
		if input.Strategy == protocol.InputMessage {
			pendingMessages = append(pendingMessages, input)
		}
	}
	initial.PendingInputs = pendingMessages
	initial.Settings = nil
	cfg.SessionID = string(row.SessionID)
	runCtx, cancel := context.WithCancel(s.root.rootContext())
	row.Run = protocol.RunView{ID: protocol.RunID("continue-" + string(c.ID)), Status: "queued", WaitPolicy: "notify"}
	if row.Purpose == childPurposeReview {
		row.Run.WaitPolicy = "observe"
	}
	next := &managedRun{workspaceLease: workspaceLease, initialInput: c.Input, initialCommandID: c.ID, fact: session.ChildRunRecorded{Version: 1, Child: row, SystemPrompt: system, CommandID: string(c.ID), CommandDigest: commandDigest}, parent: r.parent, cfg: cfg, opts: opts, lease: lease, toolLease: catalog, preHooks: hooks, cancel: cancel, done: make(chan struct{})}
	bindChildGoal(&next.fact, opts)
	u := initial.Usage
	next.fact.UsageBaseline = protocol.UsageSnapshot{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.CachedTokens, TurnCount: u.TurnCount}
	if err := s.reserveRun(next, r); err != nil {
		cancel()
		lease.Close()
		return row.Run, err
	}
	r.mu.Lock()
	next.fact.PendingMessages = append([]protocol.InputView(nil), r.fact.PendingMessages...)
	r.mu.Unlock()
	next.mu.Lock()
	err = s.persist(next, "queued")
	next.mu.Unlock()
	s.finishRunAdmission(next, err)
	if err != nil {
		cancel()
		lease.Close()
		return row.Run, err
	}
	queued = true
	go s.execute(next, runCtx, c.Text, initial)
	return row.Run, nil
}
