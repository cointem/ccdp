package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/sandbox"
)

// History returns a copy of the conversation history.
func (a *Agent) History() []messages.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]messages.Message, len(a.history))
	copy(out, a.history)
	return out
}

// MessageCount returns the number of messages in the conversation.
func (a *Agent) MessageCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.history)
}

// HistoryPreview returns a one-line preview per message for the last n messages
// (newest last), used by the interactive /rewind picker.
func (a *Agent) HistoryPreview(n int) []string {
	a.mu.Lock()
	hist := make([]messages.Message, len(a.history))
	copy(hist, a.history)
	total := len(hist)
	a.mu.Unlock()

	if n <= 0 || n > total {
		n = total
	}
	if n == 0 {
		return nil
	}
	start := total - n
	out := make([]string, 0, n)
	for i := start; i < total; i++ {
		preview := strings.ReplaceAll(hist[i].Content, "\n", " ")
		if len(preview) > 60 {
			preview = preview[:60] + "…"
		}
		out = append(out, fmt.Sprintf("%s: %s", hist[i].Role, preview))
	}
	return out
}

// ReloadSettings re-reads the per-project settings (.ccdp/settings.json +
// settings.local.json) and applies the workspace-scoped keys at runtime
// (Claude Code's settings hot-reload): permission policy, hooks, sandbox mode.
func (a *Agent) ReloadSettings() error {
	proj, err := config.LoadProjectSettings(a.cfg.Workspace)
	if err != nil {
		return err
	}
	if proj.PermissionMode != "" {
		mode, err := permissions.ParseMode(proj.PermissionMode)
		if err == nil {
			a.SetPermissionMode(mode)
		}
	}
	if len(proj.AlwaysAllow) > 0 || len(proj.AlwaysDeny) > 0 {
		a.perms.SetPolicy(permissions.Policy{
			AlwaysAllow: proj.AlwaysAllow,
			AlwaysDeny:  proj.AlwaysDeny,
		})
	}
	if len(proj.Hooks) > 0 {
		a.hooks.Update(proj.Hooks)
	}
	if proj.SandboxMode != "" {
		if mode, err := sandbox.ParseMode(proj.SandboxMode); err == nil {
			a.SetSandboxMode(mode)
		}
	}
	if proj.EnableWebTools {
		a.cfg.EnableWebTools = true
	}
	a.emitStatus("settings reloaded from .ccdp/settings.json")
	return nil
}

// SetWorkspace changes the agent's working directory at runtime (Codex /cd),
// rebuilding the sandbox and invalidating the workspace model.
func (a *Agent) SetWorkspace(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}
	a.cfg.Workspace = abs
	a.sandbox = a.cfg.Sandbox()
	a.wsInfo = nil
	a.emitStatus("workspace → %s", abs)
	return nil
}

// ExportMarkdown renders the session as a markdown transcript (Codex /export).
func (a *Agent) ExportMarkdown() string {
	a.mu.Lock()
	hist := make([]messages.Message, len(a.history))
	copy(hist, a.history)
	model, ws := a.cfg.Model, a.cfg.Workspace
	a.mu.Unlock()

	var sb strings.Builder
	fmt.Fprintf(&sb, "# ccdp session %s\n\n", a.sessionID)
	fmt.Fprintf(&sb, "- Model: %s\n- Workspace: `%s`\n- Exported: %s\n\n---\n\n",
		model, ws, time.Now().Format("2006-01-02 15:04:05"))

	for _, m := range hist {
		switch m.Role {
		case messages.RoleUser:
			fmt.Fprintf(&sb, "## User\n\n%s\n\n", m.Content)
		case messages.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n", m.Content)
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&sb, "\n_→ %s(%s)_\n", tc.Name, strings.TrimSpace(messages.MarshalArguments(tc.Arguments)))
				}
				sb.WriteString("\n")
			} else {
				fmt.Fprintf(&sb, "## Assistant\n\n%s\n\n", m.Content)
			}
		case messages.RoleTool:
			fmt.Fprintf(&sb, "### Tool result\n\n```text\n%s\n```\n\n", m.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n") + "\n"
}
