package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
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
	// PendingSettings is an admitted candidate that was not yet applied at a
	// step boundary. It is kept separate from Settings so resume cannot mistake
	// a scheduled model/policy change for the active configuration.
	PendingSettings *session.SettingsScheduled `json:"pending_settings,omitempty"`
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

func clonePendingAttachments(src map[string][]messages.ImageAttachment) map[string][]messages.ImageAttachment {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string][]messages.ImageAttachment, len(src))
	for id, attachments := range src {
		dst[id] = cloneImageAttachments(attachments)
	}
	return dst
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
			Settings: snapshotSettings(projection.Settings), PendingSettings: snapshotPendingSettings(projection.PendingSettings), PendingCommands: projection.pendingCommands(), Workflow: snapshotWorkflow(projection.Workflow),
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

// ListSessions returns both authoritative JSONL sessions and legacy flat JSON
// sessions, newest first.  It is strictly read-only: it never imports,
// creates a directory, repairs a tail, or writes a snapshot.  If both layouts
// contain an ID, the JSONL directory wins and the legacy file is omitted.
func ListSessions(dir string) ([]SessionSnapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []SessionSnapshot
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
			return nil, fmt.Errorf("agent: list session %s: %w", id, err)
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
			continue
		}
		sessions = append(sessions, *snap)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, nil
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

	a.mu.Lock()
	oldID := a.sessionID
	sourceCfg := cloneConfig(a.cfg)
	n := len(a.history)
	if keep < 0 || keep > n {
		keep = n
	}
	prefix := cloneMessages(a.history[:keep])
	suffix := cloneMessages(a.history[keep:])
	usage := a.usage
	sourceBinding := a.activeBinding
	rootCtx := a.rootCtx
	metadata := requestJournalMetadata{
		Config:           sourceCfg,
		SettingsRevision: a.settingsRev,
		ContextRevision:  a.contextRev,
		CatalogVersion:   a.catalogVersion,
		Turn:             a.turnSeq,
		Step:             a.stepSeq,
		HistoryLen:       len(a.history),
	}
	a.mu.Unlock()

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
				oldID, keep, summary),
			CreatedAt: newCreatedAt,
		})
	}
	branch = append(branch, prefix[insert:]...)
	for i := range branch {
		ensureMessageID(&branch[i])
	}

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

func (a *Agent) sessionPath() string {
	return filepath.Join(a.cfg.SessionDir, a.sessionID+".json")
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
