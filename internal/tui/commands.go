package tui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/agent"
	"ccdp/internal/permissions"
	"ccdp/internal/sandbox"
	"ccdp/internal/workspace"
)

// statuslineTokens are the valid /statusline items.
var statuslineTokens = map[string]bool{
	"version": true, "model": true, "mode": true,
	"session": true, "workspace": true, "cost": true, "context": true,
}

// commandNames is the sorted list of slash commands, parsed from commandHelp so
// it can never drift from the documented set. Used by the / autocomplete popup.
var commandNames = parseCommandNames()

func parseCommandNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(commandHelp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "/") {
			continue
		}
		// A line may hold several commands ("/quit, /exit"); each "/" token
		// contributes its name, with punctuation stripped ("/config set <k>").
		for _, tok := range strings.Fields(line) {
			if !strings.HasPrefix(tok, "/") {
				continue
			}
			name := strings.Trim(tok, "/,|")
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// commandHelp is rendered by /help.
const commandHelp = `ccdp commands
────────────────────────────────────────
/help                       show this help
/clear                      clear conversation history
/compact                    compact context now
/cost                       show token usage and estimated cost
/config                     show runtime configuration
/config set <k> <v>         change model|mode|sandbox at runtime
/doctor                     run environment diagnostics
/permissions                show permission rules
/permissions <allow|deny|remove> <rule>
                            manage persistent allow/deny rules
/memory                     show session memory (AutoMem, enable via enable_memory)
/memory clear               clear session memory
/github                     show GitHub integration status
/pr-comments                fetch review comments on the current PR
/commit-push-pr <message>   commit, push and open a PR for the current branch
/reload                     reload .ccdp/settings.json at runtime
/plugins                    show loaded plugins and LLM providers
/mcp                        show connected MCP servers, tools, resources, prompts
/skills                     list available skills
/mode                       show current permission mode
/mode <default|acceptEdits|plan|bypassPermissions>
                            switch permission mode
/model                      show current model
/model <name>               switch the active model
/plan [on|off]              toggle plan mode (propose-then-approve)
/sandbox                    show current sandbox mode
/sandbox <confine|strict|none>
                            switch sandbox mode
/remove [n]                 remove the last n messages (default 1)
/rewind [n]                 keep the first n messages; no arg = pick interactively
/checkpoint [summary|id]    create a checkpoint, or restore one by id
/review                     show changes and checkpoints
/add-dir <dir>              allow the sandbox to touch another directory
/disallowed-dir <dir>       block a directory in the sandbox
/export [path]              export the conversation as markdown
/diff [path…]               show git status and a diff of changes
/cd <dir>                   change the working directory
/pwd                        show the current working directory
/statusline                 show statusline items
/statusline <items…>        set statusline items (version model mode session workspace cost context)
/apply <file>               apply a git patch, or send a plan file to the agent
/git <args...>              run a git command in the workspace (e.g. /git status)
/save                       save the session to disk
/sessions                   list saved sessions
/resume <id>                resume a saved session
/status                     show session details
/commands                   list custom slash commands
/quit, /exit                quit ccdp

Custom commands: markdown files in ~/.ccdp/commands/<name>.md or
.ccdp/commands/<name>.md become /name; $ARGUMENTS is replaced by the args.
────────────────────────────────────────
Keys
  Enter        send message      Alt+Enter    newline (Ctrl+J works too)
  ctrl+c       interrupt agent; press twice when idle to quit
  pgup/pgdn    scroll the conversation
  ctrl+p/n     input history
  y/n          approve/deny      a/x  always allow/deny for session
  esc          clear input / dismiss
`

// runCommand executes a slash command entered in the input box.
func (m *Model) runCommand(text string) tea.Model {
	fields := strings.Fields(text)
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	args := fields[1:]

	switch cmd {
	case "help", "?":
		m.pushLog("system", commandHelp)

	case "clear":
		m.ctrl <- agent.Control{Type: agent.ControlClearHistory}
		m.items = m.items[:0]
		m.pushStatus("history cleared")

	case "compact":
		m.ctrl <- agent.Control{Type: agent.ControlCompactNow}
		m.pushStatus("compacting…")

	case "cost":
		m.pushLog("system", renderUsage(m.ag.Usage(), m.modelName))

	case "plugins":
		m.pushLog("system", renderPlugins(m.ag.PluginNames(), m.ag.ProviderNames()))

	case "mcp":
		info := m.ag.MCPInfo()
		resources := m.ag.MCPResources()
		prompts := m.ag.MCPPrompts()
		if len(info) == 0 {
			m.pushLog("system", "no MCP servers connected — configure mcp_servers in ~/.ccdp/config.json")
			return m
		}
		var sb strings.Builder
		sb.WriteString("MCP servers:\n")
		for name, names := range info {
			if len(names) == 0 {
				fmt.Fprintf(&sb, "  %s: (no tools advertised)\n", name)
			} else {
				fmt.Fprintf(&sb, "  %s tools: %s\n", name, strings.Join(names, ", "))
			}
			if rs := resources[name]; len(rs) > 0 {
				fmt.Fprintf(&sb, "    resources: %s\n", strings.Join(rs, ", "))
			}
			if ps := prompts[name]; len(ps) > 0 {
				fmt.Fprintf(&sb, "    prompts: %s\n", strings.Join(ps, ", "))
			}
		}
		m.pushLog("system", strings.TrimRight(sb.String(), "\n"))

	case "init":
		path, err := workspace.InitInstructionsFile(m.workspace)
		if err != nil {
			m.pushLog("error", "init failed: "+err.Error())
			return m
		}
		m.pushStatus("wrote " + path + " (edit it with project guidance)")

	case "mode":
		if len(args) == 0 {
			m.pushStatus(fmt.Sprintf("current permission mode: %s", m.mode))
			return m
		}
		mode, err := permissions.ParseMode(args[0])
		if err != nil {
			m.pushLog("error", err.Error())
			return m
		}
		m.ctrl <- agent.Control{Type: agent.ControlSetMode, Mode: mode}
		m.mode = mode
		m.pushStatus(fmt.Sprintf("permission mode → %s", mode))

	case "model":
		if len(args) == 0 {
			m.pushStatus("model: " + m.modelName)
			return m
		}
		m.ag.SetModel(args[0])
		m.modelName = args[0]
		m.pushStatus("model → " + args[0])

	case "plan":
		on := !m.ag.PlanMode()
		if len(args) > 0 {
			switch args[0] {
			case "on":
				on = true
			case "off":
				on = false
			default:
				m.pushLog("error", "usage: /plan [on|off]")
				return m
			}
		}
		m.ctrl <- agent.Control{Type: agent.ControlSetPlan, PlanOn: on}
		m.planMode = on
		if on {
			m.pushStatus("plan mode on — next message will produce a plan for approval")
		} else {
			m.pushStatus("plan mode off")
		}

	case "sandbox":
		if len(args) == 0 {
			m.pushStatus("sandbox mode: " + string(m.ag.SandboxMode()))
			return m
		}
		mode, err := sandbox.ParseMode(args[0])
		if err != nil {
			m.pushLog("error", err.Error())
			return m
		}
		m.ctrl <- agent.Control{Type: agent.ControlSetSandbox, SandboxMode: mode}
		m.pushStatus("sandbox mode → " + string(mode))

	case "remove":
		n := 1
		if len(args) > 0 {
			v, err := strconv.Atoi(args[0])
			if err != nil || v < 1 {
				m.pushLog("error", "usage: /remove [n] (n >= 1)")
				return m
			}
			n = v
		}
		m.ctrl <- agent.Control{Type: agent.ControlRemove, Count: n}
		m.pushStatus(fmt.Sprintf("removing last %d message(s)…", n))

	case "rewind":
		// No argument → interactive picker over the recent messages.
		if len(args) == 0 {
			total := m.ag.MessageCount()
			previews := m.ag.HistoryPreview(10)
			if len(previews) == 0 {
				m.pushLog("error", "nothing to rewind to")
				return m
			}
			lines := make([]string, len(previews))
			for i, p := range previews {
				lines[i] = p
			}
			m.startPicker("rewind to message", lines, func(sel int) {
				// sel is 0-based within the last 10; keep = total-10+sel+1.
				keep := total - len(lines) + sel + 1
				if keep < 0 {
					keep = 0
				}
				m.ctrl <- agent.Control{Type: agent.ControlRewind, Count: keep}
				m.pushStatus(fmt.Sprintf("rewinding to message %d…", keep))
			})
			return m
		}
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 {
			m.pushLog("error", "usage: /rewind [<n>] (keep the first n messages; no arg picks interactively)")
			return m
		}
		m.ctrl <- agent.Control{Type: agent.ControlRewind, Count: n}
		m.pushStatus(fmt.Sprintf("rewinding to message %d…", n))

	case "checkpoint":
		// With a known checkpoint id: restore. Otherwise: create one with the
		// given words as the summary.
		if len(args) > 0 {
			for _, r := range m.ag.CheckpointList() {
				if r.ID == args[0] {
					if err := m.ag.RestoreCheckpoint(args[0]); err != nil {
						m.pushLog("error", "restore failed: "+err.Error())
						return m
					}
					m.pushStatus("restored checkpoint " + args[0])
					return m
				}
			}
		}
		summary := "manual checkpoint"
		if len(args) > 0 {
			summary = strings.Join(args, " ")
		}
		if _, err := m.ag.CreateCheckpoint(summary); err != nil {
			m.pushLog("error", "checkpoint failed: "+err.Error())
		} else {
			m.pushStatus("checkpoint created")
		}

	case "review":
		m.runReview()

	case "add-dir":
		if len(args) == 0 {
			m.pushLog("error", "usage: /add-dir <directory>")
			return m
		}
		m.ag.AddDirectory(args[0])
		m.pushStatus("additional directory → " + args[0])

	case "disallowed-dir":
		if len(args) == 0 {
			m.pushLog("error", "usage: /disallowed-dir <directory>")
			return m
		}
		m.ag.AddDisallowedDirectory(args[0])
		m.pushStatus("disallowed directory → " + args[0])

	case "export":
		md := m.ag.ExportMarkdown()
		path := "ccdp-export-" + m.sessionID + ".md"
		if len(args) > 0 {
			path = args[0]
		}
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			m.pushLog("error", "export failed: "+err.Error())
			return m
		}
		m.pushStatus(fmt.Sprintf("exported %d messages to %s", strings.Count(md, "## "), path))

	case "diff":
		m.runDiff(args)

	case "cd":
		if len(args) == 0 {
			m.pushLog("error", "usage: /cd <dir>")
			return m
		}
		if err := m.ag.SetWorkspace(args[0]); err != nil {
			m.pushLog("error", "cd failed: "+err.Error())
			return m
		}
		m.workspace = m.ag.WorkspaceLabel()
		// Project-scoped custom commands follow the workspace.
		m.customCmds = loadCustomCommands(m.workspace)
		m.pushStatus("workspace → " + m.workspace)

	case "pwd":
		m.pushStatus("workspace: " + m.workspace)

	case "statusline":
		if len(args) == 0 {
			m.pushStatus("statusline items: " + strings.Join(m.statusItems, " "))
			return m
		}
		if args[0] == "default" {
			m.statusItems = append([]string{}, defaultStatusItems...)
			m.pushStatus("statusline reset")
			return m
		}
		var items []string
		for _, a := range args {
			if !statuslineTokens[a] {
				m.pushLog("error", fmt.Sprintf("unknown statusline item %q (valid: version model mode session workspace cost context)", a))
				return m
			}
			items = append(items, a)
		}
		m.statusItems = items
		m.pushStatus("statusline: " + strings.Join(items, " "))

	case "skills":
		names := m.ag.SkillNames()
		if len(names) == 0 {
			m.pushLog("system", "no skills — create ~/.ccdp/skills/<name>/SKILL.md or .ccdp/skills/<name>/SKILL.md")
			return m
		}
		m.pushLog("system", "skills: "+strings.Join(names, ", "))

	case "apply":
		if len(args) == 0 {
			m.pushLog("error", "usage: /apply <file.md|file.patch> — applies a plan or git patch to the workspace")
			return m
		}
		m.runApply(args[0])

	case "git":
		if len(args) == 0 {
			m.pushLog("error", "usage: /git <subcommand> [args…], e.g. /git status --short")
			return m
		}
		m.runGit(args)

	case "save":
		if err := m.ag.Save(); err != nil {
			m.pushLog("error", "save failed: "+err.Error())
		} else {
			m.pushStatus("session saved")
		}

	case "sessions":
		sessions, err := agent.ListSessions(m.ag.SessionDir())
		if err != nil {
			m.pushLog("error", err.Error())
			return m
		}
		if len(sessions) == 0 {
			m.pushLog("system", "no saved sessions")
			return m
		}
		var sb strings.Builder
		sb.WriteString("Saved sessions (newest first):\n")
		for i, s := range sessions {
			if i >= 10 {
				sb.WriteString("  …and more\n")
				break
			}
			title := s.Title
			if title == "" {
				title = "(no user message)"
			}
			fmt.Fprintf(&sb, "  %s  %s  %s\n    %s  (%d messages)\n",
				s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), s.Model, title, len(s.History))
		}
		m.pushLog("system", sb.String())

	case "resume":
		// No argument → interactive picker over saved sessions.
		if len(args) == 0 {
			m.startResumePicker()
			return m
		}
		if err := m.resumeSession(args[0]); err != nil {
			m.pushLog("error", "resume failed: "+err.Error())
			return m
		}
		m.pushStatus("resumed session " + args[0])

	case "config":
		if len(args) == 0 {
			m.pushLog("system", m.ag.ConfigSummary())
			return m
		}
		// /config set <key> <value> — runtime settable keys reuse the existing
		// commands (model, mode, sandbox); anything else is read-only.
		if args[0] == "set" && len(args) >= 3 {
			key, val := args[1], strings.Join(args[2:], " ")
			switch key {
			case "model":
				m.ag.SetModel(val)
				m.modelName = val
				m.pushStatus("model → " + val)
			case "mode":
				mode, err := permissions.ParseMode(val)
				if err != nil {
					m.pushLog("error", err.Error())
					return m
				}
				m.ctrl <- agent.Control{Type: agent.ControlSetMode, Mode: mode}
				m.mode = mode
				m.pushStatus("permission mode → " + string(mode))
			case "sandbox":
				mode, err := sandbox.ParseMode(val)
				if err != nil {
					m.pushLog("error", err.Error())
					return m
				}
				m.ctrl <- agent.Control{Type: agent.ControlSetSandbox, SandboxMode: mode}
				m.pushStatus("sandbox mode → " + string(mode))
			default:
				m.pushLog("error", "/config set supports: model, mode, sandbox (others are read-only)")
			}
			return m
		}
		m.pushLog("error", "usage: /config  |  /config set <model|mode|sandbox> <value>")

	case "doctor":
		m.pushLog("system", m.ag.Doctor())

	case "permissions":
		if len(args) == 0 {
			m.pushLog("system", m.ag.PermissionsInfo())
			return m
		}
		// /permissions allow|deny|remove <rule>
		switch args[0] {
		case "allow", "deny":
			if len(args) < 2 {
				m.pushLog("error", "usage: /permissions allow|deny <rule> (e.g. Bash:git status)")
				return m
			}
			rule := strings.Join(args[1:], " ")
			if err := m.ag.AddAlwaysRule(args[0], rule); err != nil {
				m.pushLog("error", err.Error())
			}
		case "remove":
			if len(args) < 3 {
				m.pushLog("error", "usage: /permissions remove <allow|deny> <rule>")
				return m
			}
			rule := strings.Join(args[2:], " ")
			if err := m.ag.RemoveAlwaysRule(args[1], rule); err != nil {
				m.pushLog("error", err.Error())
			}
		default:
			m.pushLog("error", "usage: /permissions [allow|deny|remove <rule>]")
		}

	case "memory":
		if len(args) > 0 && args[0] == "clear" {
			if err := m.ag.ClearMemory(); err != nil {
				m.pushLog("error", "clear failed: "+err.Error())
			} else {
				m.pushStatus("session memory cleared")
			}
			return m
		}
		text := m.ag.MemoryText()
		if text == "" {
			m.pushLog("system", "no session memory yet — set \"enable_memory\": true in ~/.ccdp/config.json to record facts learned each turn")
			return m
		}
		m.pushLog("system", text)

	case "github":
		m.pushLog("system", m.ag.GitHubStatus())

	case "pr-comments":
		m.pushLog("system", m.ag.PRComments())

	case "commit-push-pr":
		msg := strings.Join(args, " ")
		if msg == "" {
			m.pushLog("error", "usage: /commit-push-pr <commit message>")
			return m
		}
		m.pushLog("system", m.ag.CommitPushPR(msg))

	case "reload":
		if err := m.ag.ReloadSettings(); err != nil {
			m.pushLog("error", "reload failed: "+err.Error())
		} else {
			m.pushStatus("settings reloaded")
		}

	case "status":
		m.pushLog("system", fmt.Sprintf(
			"model: %s\nmode: %s\nplan: %v\nsession: %s\nworkspace: %s\ntrace: %s",
			m.modelName, m.mode, m.ag.PlanMode(), m.sessionID, m.workspace, m.ag.TracePath()))

	case "commands":
		names := m.customCommandNames()
		if len(names) == 0 {
			m.pushLog("system", "no custom commands — create ~/.ccdp/commands/<name>.md or .ccdp/commands/<name>.md ($ARGUMENTS is replaced by the arguments)")
			return m
		}
		m.pushLog("system", "custom commands: "+strings.Join(names, ", "))

	case "quit", "exit":
		m.quit = true
		return m

	default:
		// User-defined slash commands (markdown templates).
		if cc := m.findCustomCommand(cmd); cc != nil {
			m.runCustomCommand(cc, args)
			return m
		}
		m.pushLog("error", "unknown command /"+cmd+" (try /help)")
	}
	return m
}

// runDiff shows the working tree state (Codex /diff): a compact status, a diff
// stat, and optionally the full diff for the given paths. Untracked files are
// listed but not diffed (no baseline exists).
func (m *Model) runDiff(paths []string) {
	run := func(name string, args ...string) (string, bool) {
		out := &strings.Builder{}
		cmd := exec.Command("git", args...)
		cmd.Dir = m.workspace
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return strings.TrimSpace(out.String()), false
		}
		return strings.TrimRight(out.String(), "\n"), true
	}

	var sb strings.Builder
	if status, ok := run("status", "status", "--short"); ok && status != "" {
		sb.WriteString("Changed files:\n")
		sb.WriteString(status)
		sb.WriteString("\n")
	}

	diffArgs := append([]string{"diff", "--stat"}, paths...)
	if stat, ok := run("diffstat", diffArgs...); ok && stat != "" {
		sb.WriteString("\nDiff stat:\n")
		sb.WriteString(stat)
		sb.WriteString("\n")
	}

	body := append([]string{"diff"}, paths...)
	if d, ok := run("diff", body...); ok && d != "" {
		sb.WriteString("\nDiff:\n")
		if len(d) > 8000 {
			d = d[:8000] + "\n…[truncated]"
		}
		sb.WriteString(d)
		sb.WriteString("\n")
	}

	if sb.Len() == 0 {
		m.pushLog("system", "no changes in the working tree")
		return
	}
	m.pushLog("system", strings.TrimRight(sb.String(), "\n"))
}

// runGit executes git locally in the workspace and shows the output. It is a
// user-driven convenience; the agent's own Git* tools remain the primary path.
func (m *Model) runGit(args []string) {
	out := &strings.Builder{}
	cmd := exec.Command("git", args...)
	cmd.Dir = m.workspace
	cmd.Stdout = out
	cmd.Stderr = out

	done := make(chan struct{})
	if err := cmd.Start(); err != nil {
		m.pushLog("error", "git: "+err.Error())
		return
	}
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		out.WriteString("\n[git command timed out after 120s]")
	}

	m.pushLog("system", "git "+strings.Join(args, " ")+"\n"+strings.TrimRight(out.String(), "\n"))
}

// runReview shows the full review surface (Claude Code's /review): changed
// files, a diff, and any checkpoints the user can restore.
func (m *Model) runReview() {
	run := func(name string, args ...string) (string, bool) {
		out := &strings.Builder{}
		cmd := exec.Command("git", args...)
		cmd.Dir = m.workspace
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return strings.TrimSpace(out.String()), false
		}
		return strings.TrimRight(out.String(), "\n"), true
	}

	var sb strings.Builder
	if status, ok := run("status", "status", "--short"); ok && status != "" {
		sb.WriteString("Changed files:\n")
		sb.WriteString(status)
		sb.WriteString("\n")
	}
	if stat, ok := run("diffstat", "diff", "--stat"); ok && stat != "" {
		sb.WriteString("\nDiff stat:\n")
		sb.WriteString(stat)
		sb.WriteString("\n")
	}
	if d, ok := run("diff", "diff"); ok && d != "" {
		sb.WriteString("\nDiff:\n")
		if len(d) > 8000 {
			d = d[:8000] + "\n…[truncated]"
		}
		sb.WriteString(d)
		sb.WriteString("\n")
	}

	recs := m.ag.CheckpointList()
	if len(recs) > 0 {
		sb.WriteString("\nCheckpoints (restore with /checkpoint <id>):\n")
		for _, r := range recs {
			fmt.Fprintf(&sb, "  %s  %s  %s\n", r.ID, r.CreatedAt.Format("15:04:05"), r.Summary)
		}
	}

	if sb.Len() == 0 {
		m.pushLog("system", "working tree clean, no checkpoints")
		return
	}
	m.pushLog("system", strings.TrimRight(sb.String(), "\n"))
}

// startResumePicker opens the interactive saved-session selector.
func (m *Model) startResumePicker() {
	sessions, err := agent.ListSessions(m.ag.SessionDir())
	if err != nil || len(sessions) == 0 {
		m.pushLog("system", "no saved sessions to resume")
		return
	}
	lines := make([]string, 0, len(sessions))
	ids := make([]string, 0, len(sessions))
	for i, s := range sessions {
		if i >= 10 {
			break
		}
		title := s.Title
		if title == "" {
			title = "(no user message)"
		}
		lines = append(lines, fmt.Sprintf("%s  %s\n    %s  (%d messages)",
			s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), title, len(s.History)))
		ids = append(ids, s.ID)
	}
	m.startPicker("resume a session", lines, func(sel int) {
		if err := m.resumeSession(ids[sel]); err != nil {
			m.pushLog("error", "resume failed: "+err.Error())
			return
		}
		m.pushStatus("resumed session " + ids[sel])
	})
}

// resumeSession loads a saved session into the running agent and refreshes the
// conversation view.
func (m *Model) resumeSession(id string) error {
	if err := m.ag.ResumeSession(id); err != nil {
		return err
	}
	m.sessionID = id
	m.items = m.items[:0] // the history-changed event would clear it anyway
	m.pushStatus("resumed session " + id)
	return nil
}

// runApply applies a file to the workspace (Codex apply idea): git patches are
// applied with `git apply`, everything else is sent to the agent as a plan to
// execute.
func (m *Model) runApply(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		m.pushLog("error", "apply: "+err.Error())
		return
	}
	content := string(data)
	trimmed := strings.TrimSpace(content)

	// Heuristic: unified diff / git patch → git apply.
	if looksLikePatch(trimmed) {
		cmd := exec.Command("git", "apply", "--whitespace=nowarn", "-")
		cmd.Dir = m.workspace
		cmd.Stdin = strings.NewReader(content)
		out := &strings.Builder{}
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			m.pushLog("error", "git apply failed: "+strings.TrimSpace(out.String()))
			return
		}
		m.pushStatus("applied patch " + path)
		return
	}

	// A plan document: hand it to the agent to execute.
	m.ctrl <- agent.Control{Type: agent.ControlUserMessage, Text: "Execute the plan below, following it step by step.\n\n" + content}
	m.pushStatus("sent plan " + path + " to the agent")
}

// looksLikePatch reports whether text looks like a unified diff.
func looksLikePatch(text string) bool {
	if len(text) > 16*1024*1024 {
		return false
	}
	lines := strings.Split(text, "\n")
	// Accept common patch headers within the first few lines.
	probe := lines
	if len(probe) > 8 {
		probe = probe[:8]
	}
	for _, l := range probe {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "diff --git ") || strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") {
			return true
		}
	}
	return false
}

// renderUsage formats the session usage accounting for /cost.
func renderUsage(u agent.Usage, model string) string {
	var sb strings.Builder
	sb.WriteString("Usage (model " + model + "):\n")
	sb.WriteString(fmt.Sprintf("  input tokens:   %d\n", u.InputTokens))
	sb.WriteString(fmt.Sprintf("  output tokens:  %d\n", u.OutputTokens))
	if u.CachedTokens > 0 {
		sb.WriteString(fmt.Sprintf("  cached tokens:  %d\n", u.CachedTokens))
	}
	sb.WriteString(fmt.Sprintf("  total tokens:   %d\n", u.InputTokens+u.OutputTokens))
	sb.WriteString(fmt.Sprintf("  turns:          %d\n", u.TurnCount))
	sb.WriteString(fmt.Sprintf("  estimated cost: $%.4f\n", u.Cost))
	return sb.String()
}

// renderPlugins formats the loaded plugins and providers for /plugins.
func renderPlugins(plugins, providers []string) string {
	var sb strings.Builder
	sb.WriteString("Plugins (load order):\n")
	for _, p := range plugins {
		sb.WriteString("  - " + p + "\n")
	}
	sb.WriteString("LLM providers:\n")
	if len(providers) == 0 {
		sb.WriteString("  (none)\n")
	}
	for _, p := range providers {
		sb.WriteString("  - " + p + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// pushLog appends a message from the local UI (not the agent).
func (m *Model) pushLog(kind, text string) {
	m.items = append(m.items, logItem{kind: kind, text: text})
	m.render()
	m.viewport.GotoBottom()
}

// quitRequested reports whether the user asked to quit.
func (m *Model) quitRequested() bool { return m.quit }

// SessionDir delegates to the agent's session directory.
func (m *Model) SessionDir() string { return m.ag.SessionDir() }
