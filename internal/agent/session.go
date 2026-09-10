package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ccdp/internal/checkpoint"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
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
	// Persistent inbox: user messages queued while the agent was busy. They
	// are restored on resume so a crash never loses a queued message.
	Pending []string `json:"pending,omitempty"`
	// Lineage (pi's session tree): the session this one branched from, the
	// history index the branch starts at, and an LLM summary of the abandoned
	// direction.
	ParentID      string `json:"parent_id,omitempty"`
	BranchPoint   int    `json:"branch_point,omitempty"`
	BranchSummary string `json:"branch_summary,omitempty"`
}

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

// Save persists the session history to disk under the session directory.
func (a *Agent) Save() error {
	if a.cfg.NoSessionPersistence {
		return nil
	}
	a.mu.Lock()
	hist := make([]messages.Message, len(a.history))
	copy(hist, a.history)
	pending := append([]string(nil), a.pendingMsgs...)
	workspace, model := a.cfg.Workspace, a.cfg.Model
	parentID, branchPoint, branchSummary := a.parentID, a.branchPoint, a.branchSummary
	a.mu.Unlock()

	snap := SessionSnapshot{
		ID:            a.sessionID,
		CreatedAt:     a.createdAt,
		UpdatedAt:     time.Now(),
		Workspace:     workspace,
		Model:         model,
		Title:         sessionTitle(hist),
		History:       hist,
		Pending:       pending,
		ParentID:      parentID,
		BranchPoint:   branchPoint,
		BranchSummary: branchSummary,
	}
	if err := os.MkdirAll(a.cfg.SessionDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.sessionPath(), data, 0o600)
}

// LoadSession reads a session snapshot from disk.
func LoadSession(dir, id string) (*SessionSnapshot, error) {
	if dir == "" || id == "" {
		return nil, fmt.Errorf("agent: session dir and id are required")
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var snap SessionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// ResumeSession loads a saved session into this agent at runtime (used by the
// interactive session picker). History, id, timestamps and the persistent
// inbox are swapped in place. Refused while a turn is running; the trace file
// and checkpoint store are rebound to the resumed session id.
func (a *Agent) ResumeSession(id string) error {
	a.mu.Lock()
	busy := a.busy
	a.mu.Unlock()
	if busy {
		return fmt.Errorf("cannot resume while a turn is running")
	}
	snap, err := LoadSession(a.cfg.SessionDir, id)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.sessionID = snap.ID
	a.createdAt = snap.CreatedAt
	a.history = snap.History
	a.pendingMsgs = snap.Pending
	a.parentID, a.branchPoint, a.branchSummary = snap.ParentID, snap.BranchPoint, snap.BranchSummary
	a.tokenBaseline.promptTokens = 0
	a.tokenBaseline.historyLen = 0
	a.mu.Unlock()
	// Rebind per-session stores to the resumed id, mirroring Fork/Resume: the
	// trace file was still open under the previous session id, and trace
	// writes must follow TracePath().
	a.checkpoints = checkpoint.NewStore(a.cfg.SessionDir, snap.ID)
	a.closeTrace()
	a.openTrace()
	a.restoreDiscoveredFromTrace()
	a.emitSessionLifecycle(true)
	a.emit(Event{Type: EventSessionChanged, Text: id})
	a.emit(Event{Type: EventHistoryChanged, Text: "resumed session " + id})
	a.emitStatus("resumed session %s", id)
	return nil
}

// ListSessions returns saved sessions sorted newest first.
func ListSessions(dir string) ([]SessionSnapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []SessionSnapshot
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := filepath.Base(e.Name()[:len(e.Name())-len(".json")])
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

// summarizeBranch condenses the abandoned suffix of a fork into a few
// sentences. Best-effort: failures are swallowed by the caller.
func (a *Agent) summarizeBranch(span []messages.Message) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := llm.CompletionRequest{
		Model: a.activeModelSnapshot(),
		Messages: []llm.ChatMessage{
			{Role: "system", Content: branchSummarySystem},
			{Role: "user", Content: "Summarize this abandoned direction of work.\n\n--- conversation ---\n" + renderSpanView(span, 4000)},
		},
		Stream:    false,
		MaxTokens: intPtr(1024),
	}
	res, err := a.currentClient().Stream(ctx, req, nil)
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
// carries over into a fresh session id; the abandoned suffix is summarized by
// the model and attached as a system note so the new branch inherits the
// knowledge of what was tried. keep < 0 (or beyond the end) forks the full
// history. The source session is saved unchanged before the switch.
func (a *Agent) Fork(keep int) (string, error) {
	a.mu.Lock()
	busy := a.busy
	n := len(a.history)
	a.mu.Unlock()
	if busy {
		return "", fmt.Errorf("cannot fork while a turn is running")
	}
	if keep < 0 || keep > n {
		keep = n
	}

	a.mu.Lock()
	oldID := a.sessionID
	prefix := make([]messages.Message, keep)
	copy(prefix, a.history[:keep])
	suffix := make([]messages.Message, n-keep)
	copy(suffix, a.history[keep:])
	a.mu.Unlock()

	// Persist the full source session first so the fork point is durable even
	// if the agent dies right after the switch.
	if err := a.Save(); err != nil {
		return "", fmt.Errorf("save source session: %w", err)
	}

	// Summarize the abandoned direction (best effort).
	summary := ""
	if len(suffix) > 0 {
		if s, err := a.summarizeBranch(suffix); err == nil {
			summary = s
		} else {
			a.logv("branch summary failed: %v", err)
		}
	}

	newID := newSessionID()
	// The fork point can split a tool_call↔result pair (keep may land between
	// an assistant tool_calls message and its results); sanitize so the wire
	// history never carries unpaired calls (providers 400 on those).
	prefix = sanitizeToolPairs(prefix)
	branch := make([]messages.Message, 0, len(prefix)+1)
	// Insert the branch note right after the leading system prompt so
	// providers that dislike mid-conversation system roles stay happy.
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
			CreatedAt: time.Now(),
		})
	}
	branch = append(branch, prefix[insert:]...)

	a.mu.Lock()
	a.sessionID = newID
	a.createdAt = time.Now()
	a.history = branch
	a.parentID = oldID
	a.branchPoint = keep
	a.branchSummary = summary
	// History was rewritten: fall back to full local estimation until the
	// next provider-reported usage re-anchors the baseline.
	a.tokenBaseline.promptTokens = 0
	a.tokenBaseline.historyLen = 0
	a.mu.Unlock()

	// Rebind per-session stores to the new id.
	a.checkpoints = checkpoint.NewStore(a.cfg.SessionDir, newID)
	a.closeTrace()
	a.openTrace()

	// Persist the new session before announcing it, so anything reacting to
	// the events (UI, tests) can immediately load the snapshot from disk.
	if err := a.Save(); err != nil {
		a.emit(Event{Type: EventSessionChanged, Text: newID})
		a.emit(Event{Type: EventHistoryChanged, Text: fmt.Sprintf("branched session %s → %s (from message %d)", oldID, newID, keep)})
		a.emitStatus("branched into new session %s (parent %s @ message %d)", newID, oldID, keep)
		return newID, err
	}
	a.emit(Event{Type: EventSessionChanged, Text: newID})
	a.emit(Event{Type: EventHistoryChanged, Text: fmt.Sprintf("branched session %s → %s (from message %d)", oldID, newID, keep)})
	a.emitStatus("branched into new session %s (parent %s @ message %d)", newID, oldID, keep)
	return newID, nil
}

func (a *Agent) sessionPath() string {
	return filepath.Join(a.cfg.SessionDir, a.sessionID+".json")
}
