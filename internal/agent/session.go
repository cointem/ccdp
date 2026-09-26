package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// SessionSnapshot is the serializable form of a session.
type SessionSnapshot struct {
	ID        string             `json:"id"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	Workspace string             `json:"workspace"`
	Model     string             `json:"model"`
	Title     string             `json:"title,omitempty"` // derived from the first user message
	History   []messages.Message `json:"history"`
	Usage     Usage              `json:"usage"`
	// Settings and Workflow are optional typed projections. They keep public
	// session state recoverable without putting credentials or opaque runtime
	// objects in the legacy snapshot shape.
	Settings *session.Settings `json:"settings,omitempty"`
	// PendingCommands records admitted asynchronous operations with no durable
	// completion yet. Resume exposes them as unknown; it never re-runs them.
	PendingCommands []session.CommandScheduled `json:"pending_commands,omitempty"`
	Workflow        *session.WorkflowState     `json:"workflow,omitempty"`
	Memory          string                     `json:"memory,omitempty"`
	Tasks           []session.Task             `json:"tasks,omitempty"`
	// Persistent inbox: user messages queued while the agent was busy. They
	// are restored on resume so a crash never loses a queued message.
	Pending []string `json:"pending,omitempty"`
	// PendingInputs is the typed inbox projection. Pending is retained as a
	// compact compatibility view for old snapshots and callers, but new JSONL
	// snapshots carry the stable InputID and strategy so a resumed duplicate
	// can be recognized without matching message text.
	PendingInputs []protocol.InputView `json:"pending_inputs,omitempty"`
	// PendingAttachments carries only immutable blob identities in snapshots;
	// the bytes are recovered from the session ArtifactStore by request
	// construction. It is deliberately separate from the public InputView.
	PendingAttachments map[string][]messages.ImageAttachment `json:"pending_attachments,omitempty"`
	// Lineage (pi's session tree): the session this one branched from, the
	// history index the branch starts at, and an LLM summary of the abandoned
	// direction.
	ParentID      string `json:"parent_id,omitempty"`
	BranchPoint   int    `json:"branch_point,omitempty"`
	BranchSummary string `json:"branch_summary,omitempty"`
	// Sequence counters are derived from the typed log at the resume boundary.
	// They are intentionally not part of the compatibility JSON shape: a
	// snapshot loaded from an older file may omit them, and Resume re-derives
	// the authoritative values from the owned event store before admitting a
	// new turn.
	turnSeq uint64
	stepSeq uint64
}

// cloneInputViews returns an independent copy of the typed inbox projection.
// InputView currently contains only value fields, but keeping the copy helper
// here makes the snapshot boundary explicit and prevents future slice fields
// from aliasing the live Agent state.
func cloneInputViews(src []protocol.InputView) []protocol.InputView {
	if src == nil {
		return nil
	}
	return append([]protocol.InputView(nil), src...)
}

// pendingInputSnapshot aligns the compatibility text queue with its durable
// typed identities. A legacy snapshot may have only Pending, so its fallback
// IDs are deterministic and occurrence-based; once a JSONL InputQueued fact
// exists, its original ID is always retained.
func pendingInputSnapshot(sessionID string, pending []string, typed []protocol.InputView) []protocol.InputView {
	// New snapshots carry the typed queue as the source of truth. Keep it even
	// when the compact compatibility Pending text slice is absent; a queued
	// input can be empty only in malformed/legacy data, which is rejected at
	// command admission rather than silently dropped on resume.
	if len(typed) > 0 {
		out := cloneInputViews(typed)
		for i := range out {
			out[i].State = "queued"
		}
		// A legacy caller may provide additional text entries alongside a typed
		// prefix. Preserve those entries with deterministic identities, without
		// allowing a stale Pending slice to rewrite the typed bodies.
		for i := len(out); i < len(pending); i++ {
			text := pending[i]
			out = append(out, protocol.InputView{
				ID: protocol.InputID(stableID("resume-pending", struct {
					SessionID string
					Index     int
					Text      string
				}{sessionID, i, text})),
				Text: text, Strategy: protocol.InputFollowup, State: "queued",
			})
		}
		return out
	}
	if len(pending) == 0 {
		return nil
	}
	out := make([]protocol.InputView, len(pending))
	for i, text := range pending {
		out[i] = protocol.InputView{
			ID: protocol.InputID(stableID("resume-pending", struct {
				SessionID string
				Index     int
				Text      string
			}{sessionID, i, text})),
			Text: text, Strategy: protocol.InputFollowup, State: "queued",
		}
	}
	return out
}

func pendingTexts(inputs []protocol.InputView) []string {
	if len(inputs) == 0 {
		return nil
	}
	texts := make([]string, 0, len(inputs))
	for _, input := range inputs {
		texts = append(texts, input.Text)
	}
	return texts
}

// ensureTypedPendingLocked upgrades the compatibility text queue to the
// durable typed representation. Callers must hold a.mu. Runtime admission
// supplies pendingInputs directly; this bridge only fills identities for old
// callers that still mutate pendingMsgs while retaining any already durable
// IDs at matching positions.
// sessionTitle derives a short list title from the first user message.
func sessionTitle(history []messages.Message) string {
	for _, m := range history {
		if m.Role != messages.RoleUser {
			continue
		}
		line := strings.Join(strings.Fields(m.Content), " ")
		if line == "" {
			continue
		}
		if len(line) > 60 {
			line = line[:60] + "…"
		}
		return line
	}
	return ""
}

// Save flushes the current projection to the session store and refreshes its
// optional cache. It is deliberately not a history reconciliation operation:
// the event producers commit facts before publishing live state, so replay is
// the authority and Save cannot invent AssistantCommitted/SettingsChanged or
// UsageChanged events from mutable Agent fields. It never writes the legacy
// <id>.json file.
func (a *Agent) Save() error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("agent: session persistence is unavailable")
	}
	p.mu.Lock()
	if p.store == nil {
		p.mu.Unlock()
		return session.ErrClosed
	}
	if p.failed != nil {
		err := fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
		p.mu.Unlock()
		return err
	}
	projection, err := p.projectionLocked()
	if err != nil {
		p.mu.Unlock()
		return err
	}
	snapshot := snapshotFromProjection(projection, p.sessionID)
	if snapshot == nil {
		p.mu.Unlock()
		return errors.New("agent: session projection is unavailable")
	}
	snapshot.UpdatedAt = time.Now().UTC()
	p.mu.Unlock()
	if err := p.saveProjection(*snapshot); err != nil {
		// snapshot.json is only an acceleration cache; the typed event log is
		// authoritative and remains recoverable when a large history makes the
		// compatibility snapshot exceed the per-transaction bound. Do not turn
		// this expected cache miss into a poisoned runtime. Other snapshot
		// failures (I/O, cursor conflicts, corruption) remain visible to the
		// caller and retain their existing fail-closed handling.
		if errors.Is(err, session.ErrTooLarge) {
			a.emitStatus("session snapshot cache skipped: %v", err)
			return nil
		}
		return err
	}
	return nil
}

// LoadSession reads a session snapshot without mutating the session tree.  A
// new JSONL log is authoritative when present; the legacy flat JSON file is
// consulted only when no new log exists.
func LoadSession(dir, id string) (*SessionSnapshot, error) {
	snapshot, _, err := loadSessionSnapshot(dir, id)
	return snapshot, err
}

// OpenSession prepares an independent Agent handle for a saved session. It
// never mutates this Agent: the caller can atomically replace its UI handle
// only after OpenSession succeeds, then close the old handle. Legacy JSON
// import, when needed, is performed through the explicit Resume boundary.
func (a *Agent) OpenSession(id string) (*Agent, error) {
	if a == nil || a.cfg == nil {
		return nil, errors.New("agent: cannot open a session from a nil agent")
	}
	a.mu.Lock()
	busy, closing := a.busy, a.closing
	// Clone all mutable config state while holding the Agent lock. The opened
	// handle must not share provider maps, policy slices, or session channels
	// with the source; a late command on the old handle must never be consumed
	// by the new session.
	cfg := cloneConfig(a.cfg)
	a.mu.Unlock()
	if busy {
		return nil, errors.New("agent: cannot open a session while a turn is running")
	}
	if closing {
		return nil, errors.New("agent: cannot open a session while closing")
	}
	snapshot, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		return nil, err
	}
	return Resume(&cfg, snapshot, nil)
}

func loadSessionSnapshot(dir, id string) (*SessionSnapshot, bool, error) {
	if dir == "" || id == "" {
		return nil, false, fmt.Errorf("agent: session dir and id are required")
	}
	if err := validateSessionID(id); err != nil {
		return nil, false, err
	}
	newEvents := filepath.Join(dir, id, "events.v1.jsonl")
	_, statErr := os.Stat(newEvents)
	if statErr == nil {
		store, err := session.OpenJSONLReadOnly(dir, id)
		if err != nil {
			return nil, false, err
		}
		records, readErr := session.ReadAll(store)
		closeErr := store.Close()
		if readErr != nil {
			return nil, false, readErr
		}
		if closeErr != nil {
			return nil, false, closeErr
		}
		if legacyImportEvidence(records) && !legacyImportCompletePresent(records) {
			// A partially imported log is not authoritative. Keep the old file as
			// the read-only view until an explicit Resume completes the import.
			legacyPath := filepath.Join(dir, id+".json")
			if _, legacyErr := os.Stat(legacyPath); legacyErr == nil {
				snapshot, loadErr := loadLegacySnapshotFile(legacyPath, id)
				if loadErr != nil {
					return nil, false, loadErr
				}
				return snapshot, true, nil
			} else if !errors.Is(legacyErr, os.ErrNotExist) {
				return nil, false, legacyErr
			}
			return nil, false, fmt.Errorf("agent: session %q has an incomplete legacy import", id)
		}
		projection, projectionErr := projectRecords(records)
		if projectionErr != nil {
			return nil, false, projectionErr
		}
		if projection.ID == "" {
			projection.ID = id
		}
		if projection.ID != id {
			return nil, false, fmt.Errorf("agent: replayed session id %q does not match requested id %q", projection.ID, id)
		}
		snapshot := &SessionSnapshot{
			ID: projection.ID, CreatedAt: projection.CreatedAt, UpdatedAt: projection.updatedAt,
			Workspace: projection.Workspace, Model: projection.Model, Title: sessionTitle(projection.History),
			History: cloneMessages(projection.History), Usage: projection.Usage, Pending: append([]string(nil), projection.Pending...), PendingInputs: projection.pendingInputViews(), PendingAttachments: projection.pendingAttachments(),
			Settings: snapshotSettings(projection.Settings), PendingCommands: projection.pendingCommands(), Workflow: snapshotWorkflow(projection.Workflow),
			Memory: projection.MemoryText, Tasks: append([]session.Task(nil), projection.Tasks...),
			ParentID: projection.ParentID, BranchPoint: projection.BranchPoint, BranchSummary: projection.BranchSummary,
			turnSeq: projection.turnSeq, stepSeq: projection.stepSeq,
		}
		return snapshot, false, nil
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, false, statErr
	}
	path := filepath.Join(dir, id+".json")
	snap, err := loadLegacySnapshotFile(path, id)
	if err != nil {
		return nil, true, err
	}
	return snap, true, nil
}

func loadLegacySnapshotFile(path, id string) (*SessionSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap SessionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	if err := validateSessionID(snap.ID); err != nil {
		return nil, err
	}
	if snap.ID != id {
		return nil, fmt.Errorf("agent: session file id %q does not match requested id %q", snap.ID, id)
	}
	// Old flat snapshots only carried the compatibility text queue. Assign
	// deterministic in-memory identities at this read boundary; this does not
	// write or import the legacy file. Explicit Resume will persist the durable
	// InputQueued facts with these identities.
	snap.PendingInputs = pendingInputSnapshot(snap.ID, snap.Pending, snap.PendingInputs)
	return &snap, nil
}

func validateSessionID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("agent: invalid session id %q", id)
	}
	return nil
}

// SessionListIssue records one session directory ListSessions had to leave out.
// A damaged or unreadable log must not hide every other session, but the
// omission has to be visible: without it an empty list is indistinguishable
// from "nothing to resume".
type SessionListIssue struct {
	ID     string
	Reason string
}

// ListSessions returns both authoritative JSONL sessions and legacy flat JSON
// sessions, newest first, plus an issue for every session it had to skip.  It is
// strictly read-only: it never imports, creates a directory, repairs a tail, or
// writes a snapshot.  If both layouts contain an ID, the JSONL directory wins and
// the legacy file is omitted.
func ListSessions(dir string) ([]SessionSnapshot, []SessionListIssue, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var sessions []SessionSnapshot
	var issues []SessionListIssue
	newIDs := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if validateSessionID(id) != nil {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(dir, id, "events.v1.jsonl")); statErr != nil {
			continue
		}
		snap, legacy, err := loadSessionSnapshot(dir, id)
		if err != nil {
			issues = append(issues, SessionListIssue{ID: id, Reason: err.Error()})
			continue
		}
		if legacy {
			// A non-authoritative/incomplete import is represented by the legacy
			// file below; do not hide it behind the unfinished directory.
			continue
		}
		newIDs[id] = true
		if info, infoErr := os.Stat(filepath.Join(dir, id, "events.v1.jsonl")); infoErr == nil && info.ModTime().After(snap.UpdatedAt) {
			snap.UpdatedAt = info.ModTime()
		}
		sessions = append(sessions, *snap)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if validateSessionID(id) != nil || newIDs[id] {
			continue
		}
		snap, err := LoadSession(dir, id)
		if err != nil {
			issues = append(issues, SessionListIssue{ID: id, Reason: err.Error()})
			continue
		}
		sessions = append(sessions, *snap)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, issues, nil
}

// ---------------------------------------------------------------------------
// Session branching (pi's append-only session tree, adapted to snapshots)
// ---------------------------------------------------------------------------

// Agent lineage accessors (guarded by mu). Set by Fork; persisted by Save.
// parentID is the session this one branched from, branchPoint the history
// index the branch starts at, branchSummary an LLM summary of the abandoned
// direction so the new branch knows what was already tried.
func (a *Agent) Lineage() (parentID string, branchPoint int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.parentID, a.branchPoint
}

// branchSummarySystem is the summarization prompt for an abandoned branch:
// unlike compaction, the goal is "what was tried so it is not repeated".
const branchSummarySystem = `You summarize an abandoned direction of work so a fresh
agent branch knows what was already tried without repeating it. In dense
factual prose: the goal, what was done and attempted, and the outcome or
blockers. Do NOT call any tool. Output the summary only.`

// summarizeBranchFrozen condenses the abandoned suffix with one immutable
// provider binding and the source root context captured by Fork. Keeping the
// provider and model together prevents a concurrent model switch from mixing
// request metadata with the wrong provider. It uses the same prepared-call and
// journal boundary as main turns and compaction.
func (a *Agent) summarizeBranchFrozen(span []messages.Message, binding modelBinding, rootCtx context.Context, metadata requestJournalMetadata) (string, error) {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	if binding.client == nil {
		return "", errors.New("agent: fork summary provider is unavailable")
	}
	ctx, cancel := context.WithTimeout(rootCtx, 2*time.Minute)
	defer cancel()
	req := llm.CompletionRequest{
		ReasoningEffort: metadata.Config.ReasoningEffort,
		Verbosity:       metadata.Config.Verbosity,
		Model:           binding.model,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: branchSummarySystem},
			{Role: "user", Content: "Summarize this abandoned direction of work.\n\n--- conversation ---\n" + renderSpanView(span, 4000)},
		},
		Stream:    true,
		MaxTokens: intPtr(1024),
	}
	applyGenerationRequest(metadata.Config, &req)
	call, err := prepareProviderCall(binding, req)
	if err != nil {
		return "", err
	}
	res, err := a.streamPrepared(ctx, "fork", call, metadata, nil, nil)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Text)
	if out == "" {
		return "", fmt.Errorf("empty branch summary")
	}
	return out, nil
}

// Fork branches a new session from this one (pi's session tree): history[:keep]
// is copied into a fresh JSONL session and the abandoned suffix is summarized
// by the model. Fork never switches the source Agent; callers can explicitly
// open the returned child later once the UI is ready to replace its handle.
func (a *Agent) Fork(keep int) (string, error) {
	a.mu.Lock()
	busy, closing := a.busy, a.closing || a.closed
	a.mu.Unlock()
	if busy {
		return "", fmt.Errorf("cannot fork while a turn is running")
	}
	if closing {
		return "", fmt.Errorf("cannot fork while session is closing")
	}
	// Persist the full source session first so the fork point is durable. Save
	// may assign stable IDs to legacy in-memory messages, but never changes the
	// source's logical history, session identity, or resource ownership.
	if err := a.Save(); err != nil {
		return "", fmt.Errorf("save source session: %w", err)
	}

	oldID, sourceCfg, prefix, suffix, usage, sourceBinding, rootCtx, metadata, keep := a.snapshotForkSource(keep)

	// Summarization is best effort for provider errors, but cancellation of the
	// source root context is a hard stop: do not publish a child with a silently
	// missing summary after the caller has begun shutdown.
	summary := ""
	if len(suffix) > 0 {
		if s, err := a.summarizeBranchFrozen(suffix, sourceBinding, rootCtx, metadata); err == nil {
			summary = s
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (rootCtx != nil && rootCtx.Err() != nil) {
			return "", fmt.Errorf("summarize fork: %w", err)
		} else if isRequestJournalFailure(err) {
			// Journal admission/finish is part of the fork operation's
			// correctness boundary. Do not create a branch after either fact
			// failed, even though ordinary provider summary errors remain best
			// effort for backwards compatibility.
			return "", fmt.Errorf("summarize fork journal: %w", err)
		} else {
			a.logv("branch summary failed: %v", err)
		}
	}
	if rootCtx != nil {
		if err := rootCtx.Err(); err != nil {
			return "", fmt.Errorf("fork canceled: %w", err)
		}
	}

	newID := newSessionID()
	newCreatedAt := time.Now().UTC()
	childCfg := sourceCfg
	childCfg.SessionID = newID
	childPersistence, err := openSessionPersistence(&childCfg, newID, newCreatedAt)
	if err != nil {
		return "", fmt.Errorf("open branched session: %w", err)
	}
	closeChild := func(operationErr error) error {
		closeErr := childPersistence.close()
		if operationErr != nil {
			return errors.Join(operationErr, closeErr)
		}
		return closeErr
	}
	// The fork point can split a tool_call↔result pair (keep may land between
	// an assistant tool_calls message and its results); sanitize the frozen
	// child history so its first provider request is valid.
	branch := buildForkBranch(prefix, summary, oldID, keep, newCreatedAt)

	if _, err := childPersistence.commitEvents("fork-"+newID, session.SessionImported{
		SessionID: newID, FormatVersion: session.SchemaVersion, Source: "fork", OriginalPath: oldID,
		ImportedAt: newCreatedAt, ParentID: oldID, BranchPoint: keep, BranchSummary: summary,
	}); err != nil {
		return "", closeChild(fmt.Errorf("persist branch metadata: %w", err))
	}
	if err := childPersistence.persistHistoryMessages(branch, "fork-"+newID); err != nil {
		return "", closeChild(fmt.Errorf("persist branch history: %w", err))
	}
	if err := childPersistence.persistUsage(usage); err != nil {
		return "", closeChild(fmt.Errorf("persist branch usage: %w", err))
	}
	childSnapshot := SessionSnapshot{
		ID: newID, CreatedAt: newCreatedAt, UpdatedAt: newCreatedAt,
		Workspace: childCfg.Workspace, Model: childCfg.Model,
		Title: sessionTitle(branch), History: cloneMessages(branch), Usage: usage,
		ParentID: oldID, BranchPoint: keep, BranchSummary: summary,
	}
	if err := childPersistence.saveProjection(childSnapshot); err != nil {
		return "", closeChild(fmt.Errorf("save branch projection: %w", err))
	}
	if err := childPersistence.close(); err != nil {
		return "", fmt.Errorf("close branched session: %w", err)
	}
	return newID, nil
}

// snapshotForkSource captures the immutable source-session state a branch needs
// under one lock: identity, config, split history spans, usage, binding, root
// context, and the request-journal metadata. keep is clamped to the history
// bounds.
func (a *Agent) snapshotForkSource(keep int) (oldID string, sourceCfg config.Config, prefix, suffix []messages.Message, usage Usage, sourceBinding modelBinding, rootCtx context.Context, metadata requestJournalMetadata, clampedKeep int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	oldID = a.sessionID
	sourceCfg = cloneConfig(a.cfg)
	n := len(a.history)
	if keep < 0 || keep > n {
		keep = n
	}
	prefix = cloneMessages(a.history[:keep])
	suffix = cloneMessages(a.history[keep:])
	usage = a.usage
	sourceBinding = a.activeBinding
	rootCtx = a.rootCtx
	metadata = requestJournalMetadata{
		Config:           sourceCfg,
		SettingsRevision: a.settingsRev,
		ContextRevision:  a.contextRev,
		CatalogVersion:   a.catalogVersion,
		Turn:             a.turnSeq,
		Step:             a.stepSeq,
		HistoryLen:       len(a.history),
	}
	return oldID, sourceCfg, prefix, suffix, usage, sourceBinding, rootCtx, metadata, keep
}

// buildForkBranch assembles the child history: the leading system facts, an
// injected branch-summary system message, and the kept prefix. The tool_call↔
// result boundary is sanitized so the child's first provider request is valid.
func buildForkBranch(prefix []messages.Message, summary, oldID string, branchPoint int, newCreatedAt time.Time) []messages.Message {
	prefix = sanitizeToolPairs(prefix)
	branch := make([]messages.Message, 0, len(prefix)+1)
	insert := 0
	if len(prefix) > 0 && prefix[0].Role == messages.RoleSystem {
		insert = 1
	}
	branch = append(branch, prefix[:insert]...)
	if summary != "" {
		branch = append(branch, messages.Message{
			Role: messages.RoleSystem,
			Content: fmt.Sprintf(
				"This session was branched from session %s before message %d. Work continued from here in a different direction.\n\nSummary of the abandoned direction (already tried — do not repeat unless asked):\n\n%s",
				oldID, branchPoint, summary),
			CreatedAt: newCreatedAt,
		})
	}
	branch = append(branch, prefix[insert:]...)
	for i := range branch {
		ensureMessageID(&branch[i])
	}
	return branch
}

// ExportSessionMarkdown renders a replayed snapshot without constructing an
// Agent, opening providers/MCP, acquiring a writer lock, or running hooks.
// It is used by the CLI's read-only replay mode.
func ExportSessionMarkdown(snapshot *SessionSnapshot) string {
	if snapshot == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# ccdp session %s\n\n", snapshot.ID)
	fmt.Fprintf(&sb, "- Model: %s\n- Workspace: `%s`\n- Exported: %s\n\n---\n\n",
		snapshot.Model, snapshot.Workspace, time.Now().Format("2006-01-02 15:04:05"))
	for _, message := range snapshot.History {
		switch message.Role {
		case messages.RoleUser:
			fmt.Fprintf(&sb, "## User\n\n%s\n\n", message.Content)
		case messages.RoleAssistant:
			if len(message.ToolCalls) > 0 {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n", message.Content)
				for _, call := range message.ToolCalls {
					fmt.Fprintf(&sb, "\n_→ %s(%s)_\n", call.Name, strings.TrimSpace(messages.MarshalArguments(call.Arguments)))
				}
				sb.WriteString("\n")
			} else {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n\n", message.Content)
			}
		case messages.RoleTool:
			fmt.Fprintf(&sb, "### Tool result\n\n```text\n%s\n```\n\n", message.Content)
		case messages.RoleSystem:
			fmt.Fprintf(&sb, "## System\n\n%s\n\n", message.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n") + "\n"
}

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
		r.mu.Unlock()
		return view, err
	}
	view := r.final
	row := r.fact.Child
	r.mu.Unlock()
	if view.SessionID != "" {
		view.RunID = row.Run.ID
		// A closed runtime is still a readable session. Preserve its settings,
		// but expose the settled run as idle and load the bounded transcript.
		view.Busy, view.Closing, view.Phase = false, false, protocol.PhaseIdle
		view.Approval, view.Question, view.Plan = nil, nil, nil
		if view.Transcript == nil {
			page, err := c.supervisor.ReadTranscript(ctx, row.SessionID, 0, transcriptWindow)
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
	if row.Run.Active() {
		view.Phase = protocol.PhasePreparing
	}
	page, err := c.supervisor.ReadTranscript(ctx, row.SessionID, 0, transcriptWindow)
	if err != nil && !errors.Is(err, os.ErrNotExist) && row.Run.Status != "queued" && row.Run.Status != "starting" {
		return view, err
	}
	view.Transcript, view.TranscriptMore = page.Items, page.More
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
					Revision:        changed.Revision,
					Model:           protocol.ModelBinding{Model: s.Model, Provider: s.Provider, Endpoint: s.Endpoint},
					ExecutionMode:   protocol.ExecutionMode(s.ExecutionMode),
					Permission:      protocol.PermissionPolicy{Mode: s.PermissionPolicy, AlwaysAllow: s.AlwaysAllow, AlwaysDeny: s.AlwaysDeny, Revision: changed.Revision},
					Sandbox: protocol.SandboxPolicy{NetworkAccess: s.NetworkAccess,
						AdditionalDirectories: s.AdditionalDirectories,
						AdditionalReadOnlyDirectories: s.AdditionalReadOnlyDirectories,
						DisallowedDirectories: s.DisallowedDirectories, Revision: changed.Revision},
					ReasoningEffort: s.ReasoningEffort, Verbosity: s.Verbosity,
					ContextWindow: window, CompactThreshold: s.CompactThreshold,
					MaxOutputTokens: maxOutputTokens, MaxTurns: s.MaxTurns, MaxBudgetUSD: s.MaxBudgetUSD,
				}
			}
			view.Revision.LogSeq = uint64(record.Seq)
		}
	}
	return view, nil
}

func (c *managedClient) Watch(ctx context.Context, cursor protocol.Cursor) (protocol.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	view, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	sub := &managedSubscription{ch: make(chan protocol.Update, 64), cancel: cancel, done: make(chan struct{})}
	sub.ch <- protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &view}
	go c.observe(life, sub)
	return sub, nil
}

// Observation follows the session across queued, running and dormant states.
// It owns only its subscriptions; stopping it cannot close or cancel an Agent.
func (c *managedClient) observe(ctx context.Context, out *managedSubscription) {
	defer close(out.done)
	defer close(out.ch)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastRun protocol.RunID
	var lastStatus string
	var live protocol.Subscription
	var updates <-chan protocol.Update
	var attached *Agent
	var snapshotAgent *Agent
	defer func() {
		if live != nil {
			_ = live.Close()
		}
	}()
	send := func(update protocol.Update) bool {
		if len(out.ch) >= cap(out.ch)-1 {
			out.ch <- protocol.Update{Type: protocol.UpdateResyncRequired, Cursor: update.Cursor, Revision: update.Revision}
			return false
		}
		select {
		case out.ch <- update:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		r := c.current()
		r.mu.Lock()
		a, row := r.agent, r.fact.Child
		r.mu.Unlock()
		if a != attached || row.Run.ID != lastRun {
			if live != nil {
				_ = live.Close()
				live = nil
				updates = nil
			}
			attached = a
			if a != nil {
				var err error
				live, err = a.Watch(ctx, protocol.Cursor{})
				if err == nil {
					updates = live.Updates()
				} else {
					attached = nil
				}
			}
		}
		// Detaching a settled runtime changes the session projection to idle,
		// even when its run status was already terminal before cleanup.
		if lastRun != row.Run.ID || lastStatus != row.Run.Status || snapshotAgent != a {
			snapshot, err := c.Snapshot(ctx)
			if err != nil {
				return
			}
			if !send(protocol.Update{Type: protocol.UpdateSnapshot, Snapshot: &snapshot, Revision: snapshot.Revision}) {
				return
			}
			lastRun, lastStatus = row.Run.ID, row.Run.Status
			snapshotAgent = a
		}
		select {
		case <-ctx.Done():
			return
		case <-c.supervisor.root.rootCtx.Done():
			return
		case <-ticker.C:
		case update, ok := <-updates:
			if !ok {
				live = nil
				updates = nil
				attached = nil
				continue
			}
			if update.Snapshot != nil {
				copy := *update.Snapshot
				copy.RunID = lastRun
				update.Snapshot = &copy
			}
			if !send(update) || update.Type == protocol.UpdateResyncRequired {
				return
			}
		}
	}
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
		r.mu.Lock()
		settled := !r.fact.Child.Run.Active()
		ownContinuation := r.fact.CommandID == string(cmd.ID)
		r.mu.Unlock()
		if settled || ownContinuation {
			_, err := c.supervisor.Control(ctx, protocol.AgentControl{ID: cmd.ID, SessionID: cmd.SessionID, RunID: cmd.ExpectedRunID, Action: "continue", Text: cmd.Input.Text, Input: cmd.Input})
			if err != nil {
				return protocol.Receipt{}, err
			}
			return protocol.Receipt{CommandID: cmd.ID, SessionID: cmd.SessionID, Status: protocol.ReceiptScheduled}, nil
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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
		if row.Purpose != childPurposeTask {
			return protocol.Receipt{}, errors.New("guardian is read-only")
		}
	case protocol.CommandApproveTool:
		if r.opts.NonInteractive {
			return protocol.Receipt{}, errors.New("approval channel unavailable")
		}
	case protocol.CommandInterrupt, protocol.CommandAnswerQuestion, protocol.CommandApprovePlan:
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
	return r.agent.Submit(ctx, cmd)
}

type managedSubscription struct {
	ch     chan protocol.Update
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *managedSubscription) Updates() <-chan protocol.Update { return s.ch }
func (s *managedSubscription) Close() error                    { s.cancel(); <-s.done; return nil }

func (s *SessionSupervisor) ReadTranscript(ctx context.Context, id protocol.SessionID, before uint64, limit int) (protocol.TranscriptPage, error) {
	if limit <= 0 || limit > transcriptWindow {
		limit = transcriptWindow
	}
	records, _, err := s.readSessionRecords(ctx, id)
	if err != nil {
		return protocol.TranscriptPage{}, err
	}
	// Keep only the requested page's text, not an unbounded text projection.
	// The compact identity index preserves stable pagination when later facts
	// complete tools that started before this page.
	t := &transcriptState{index: make(map[string]uint64), before: before, window: limit}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return protocol.TranscriptPage{}, err
		}
		t.facts([]session.Event{record.Event})
	}
	items, _ := t.snapshot()
	var cursor uint64
	if len(items) > 0 {
		cursor = t.index[items[0].ID]
	}
	return protocol.TranscriptPage{Items: items, Before: cursor, More: cursor > 1}, nil
}

type outputReader interface {
	Read(session.BlobRef, int64) ([]byte, error)
}

func (s *SessionSupervisor) readSessionRecords(ctx context.Context, id protocol.SessionID) ([]session.Record, outputReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var p *sessionPersistence
	if id == protocol.SessionID(s.root.sessionID) {
		p = s.root.persistenceHandle()
	} else {
		r, err := s.lookup(id)
		if err != nil {
			return nil, nil, err
		}
		r.mu.Lock()
		id = r.fact.Child.SessionID
		if r.agent != nil {
			p = r.agent.persistenceHandle()
		}
		if p == nil && r.memory != nil {
			records, blobs := append([]session.Record(nil), r.memory...), r.memoryBlobs
			r.mu.Unlock()
			return records, blobs, nil
		}
		r.mu.Unlock()
	}
	if p != nil {
		// Keep the writer/reader handle alive for this read. Closing the owner
		// may detach it, but cannot invalidate the copied immutable records.
		p.mu.Lock()
		var blobs outputReader = p.artifacts
		if p.memoryArtifacts != nil {
			blobs = p.memoryArtifacts
		}
		var records []session.Record
		var err error
		if p.store != nil {
			records, err = p.store.Read(session.Beginning)
		} else {
			err = session.ErrClosed
		}
		p.mu.Unlock()
		if !errors.Is(err, session.ErrClosed) {
			return records, blobs, err
		}
	}
	store, err := session.OpenJSONLReadOnly(s.sessionDir, string(id))
	if err != nil {
		return nil, nil, err
	}
	defer store.Close()
	records, err := store.Read(session.Beginning)
	return records, session.OpenArtifactStoreReadOnly(filepath.Join(s.sessionDir, string(id))), err
}

func (s *SessionSupervisor) ReadOutput(ctx context.Context, id protocol.SessionID, itemID string, offset int64, limit int) (protocol.OutputPage, error) {
	if itemID == "" {
		return protocol.OutputPage{}, errors.New("item_id required; read session history first to select an item")
	}
	if offset < 0 {
		return protocol.OutputPage{}, errors.New("negative output offset")
	}
	if limit <= 0 || limit > 64<<10 {
		limit = 64 << 10
	}
	records, blobs, err := s.readSessionRecords(ctx, id)
	if err != nil {
		return protocol.OutputPage{}, err
	}
	var text string
	var ref *session.BlobRef
	found := false
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return protocol.OutputPage{}, err
		}
		switch e := record.Event.(type) {
		case *session.InputQueued:
			messageID := e.MessageID
			if messageID == "" {
				messageID = inputMessageID(e.InputID)
			}
			if messageID == itemID {
				text = e.Text
				found = true
			}
		case *session.AssistantCommitted:
			if "reasoning:"+e.Message.MessageID == itemID && e.Message.ReasoningContent != "" {
				text = e.Message.ReasoningContent
				found = true
			}
			for _, block := range e.Message.Content {
				if result := block.ToolResult; result != nil && toolTranscriptID(e.TurnID, e.StepID, result.CallID) == itemID && !found {
					text = result.Text
					ref = result.Blob
					found = true
				}
			}
			if e.Message.MessageID == itemID {
				found = true
				text = ""
				for _, block := range e.Message.Content {
					if block.Kind == session.ContentText {
						text += block.Text
					}
				}
			}
		case *session.ToolFinished:
			if toolTranscriptID(e.TurnID, e.StepID, e.CallID) == itemID {
				text = e.Result.Text
				ref = e.RawOutput
				if ref == nil {
					ref = e.Result.Blob
				}
				found = true
			}
		case *session.TurnFinished:
			if "error:"+e.TurnID == itemID {
				text = e.Error
				found = true
			}
		}
	}
	if !found {
		return protocol.OutputPage{}, fmt.Errorf("saved transcript item %q not found", itemID)
	}
	if ref != nil {
		if blobs == nil {
			return protocol.OutputPage{}, errors.New("output artifact unavailable")
		}
		data, err := blobs.Read(*ref, 64<<20)
		if err != nil {
			return protocol.OutputPage{}, err
		}
		text = string(data)
	}
	if offset > int64(len(text)) {
		return protocol.OutputPage{}, errors.New("offset exceeds output length")
	}
	start := int(offset)
	if start < len(text) && !utf8.RuneStart(text[start]) {
		return protocol.OutputPage{}, errors.New("offset is not a UTF-8 boundary")
	}
	end := min(len(text), start+limit)
	for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == start && start < len(text) {
		_, n := utf8.DecodeRuneInString(text[start:])
		end = start + n
	}
	return protocol.OutputPage{Text: text[start:end], Next: int64(end), Total: int64(len(text)), More: end < len(text)}, nil
}

// Called after the run has joined its workers but before releasing its writer.
// Cancellation is durable so a later continuation cannot drain old inputs.
func (a *Agent) cancelRunInputs(runID string) error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.mu.Lock()
	a.ensureTypedPendingLocked()
	var facts []session.Event
	for _, input := range a.pendingInputs {
		facts = append(facts, session.InputCancelled{InputID: string(input.ID), Reason: "run settled"})
	}
	a.mu.Unlock()
	if len(facts) == 0 {
		return nil
	}
	if _, err := a.persistenceHandle().commitEvents("run-cancel-inputs-"+runID, facts...); err != nil {
		return err
	}
	a.mu.Lock()
	a.pendingInputs, a.pendingMsgs = nil, nil
	a.mu.Unlock()
	return nil
}

func childRunOutput(a *Agent, previous map[string]bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A failed continuation must never return a previous run's answer.
	for i := len(a.history) - 1; i >= 0; i-- {
		if a.history[i].Role == messages.RoleAssistant && a.history[i].Content != "" && !previous[a.history[i].ID] {
			return a.history[i].Content
		}
	}
	return ""
}
