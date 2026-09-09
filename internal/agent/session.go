package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	workspace, model := a.cfg.Workspace, a.cfg.Model
	a.mu.Unlock()

	snap := SessionSnapshot{
		ID:        a.sessionID,
		CreatedAt: a.createdAt,
		UpdatedAt: time.Now(),
		Workspace: workspace,
		Model:     model,
		Title:     sessionTitle(hist),
		History:   hist,
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
// interactive session picker). History, id and timestamps are swapped in place.
func (a *Agent) ResumeSession(id string) error {
	snap, err := LoadSession(a.cfg.SessionDir, id)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.sessionID = snap.ID
	a.createdAt = snap.CreatedAt
	a.history = snap.History
	a.mu.Unlock()
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

func (a *Agent) sessionPath() string {
	return filepath.Join(a.cfg.SessionDir, a.sessionID+".json")
}
