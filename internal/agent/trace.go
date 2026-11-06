package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"ccdp/internal/protocol"
)

// openTrace creates the per-session diagnostics stream, separate from replay.
func (a *Agent) openTrace() {
	if a.services.cfg.SessionDir == "" || a.services.cfg.NoSessionPersistence {
		return
	}
	path := a.TracePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path,
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	a.trace = f
}

// writeTrace records the same event delivered to Watch, with a diagnostic timestamp.
// The typed session journal remains the only replay authority.
func (a *Agent) writeTrace(ev protocol.EventView) {
	a.traceMu.Lock()
	defer a.traceMu.Unlock()
	if a.trace == nil {
		return
	}
	rec := struct {
		TS string `json:"ts"`
		protocol.EventView
	}{time.Now().Format(time.RFC3339Nano), ev}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = a.trace.Write(append(b, '\n'))
}

// closeTrace flushes and closes the trace file. The nil check and the close
// both happen under traceMu so it cannot race with a running turn's
// writeTrace (Close may be called while a turn is still streaming).
func (a *Agent) closeTrace() {
	a.traceMu.Lock()
	if a.trace != nil {
		_ = a.trace.Close()
		a.trace = nil
	}
	a.traceMu.Unlock()
}

// TracePath returns the diagnostics path; restore never reads this file.
func (a *Agent) TracePath() string {
	if a.services.cfg.SessionDir == "" || a.conversation.sessionID == "" {
		return ""
	}
	return filepath.Join(a.services.cfg.SessionDir, a.conversation.sessionID, "diagnostics.jsonl")
}
