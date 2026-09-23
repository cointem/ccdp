package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/commands"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
)

// statuslineTokens are the valid /statusline items.
var statuslineTokens = map[string]bool{
	"version": true, "model": true, "mode": true,
	"session": true, "workspace": true, "cost": true, "context": true,
	"effort": true,
}

// commandCatalog is the one source of command metadata. The autocomplete
// list is derived from it, so command parsing never depends on rendered help.
var commandCatalog = commands.Default()

var commandNames = commandCatalog.Names()

// runCommand executes a slash command entered in the input box. Commands that
// need the tea runtime (e.g. quitting) return a tea.Cmd alongside the model.
func (m *Model) runCommand(text string) (tea.Model, tea.Cmd) {
	fields, err := commands.ParseLine(text)
	if err != nil {
		m.pushLog("error", "command parse failed: "+err.Error())
		return m, nil
	}
	if len(fields) == 0 {
		return m, nil
	}
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	args := fields[1:]
	if canonical, ok := commandCatalog.Canonical(cmd); ok {
		cmd = canonical
	}
	if m.routing != nil && m.sessionID != m.routing.rootID {
		switch cmd {
		case "pending", "tasks", "next", "reconnect", "agents", "agent", "agent-history", "agent-output", "transcript", "parent", "root", "help", "cost", "context", "copy", "stop-tree", "quit":
		default:
			m.pushStatus("this command belongs to the main session; use /root first")
			return m, nil
		}
	}
	if entry, ok := commandCatalog.Lookup(cmd); ok && m.busy && entry.Busy == commands.BusyReject {
		m.pushStatus("/" + entry.Name + " is unavailable while the agent is busy")
		return m, nil
	}

	switch cmd {
	case "reconnect":
		return m, m.reconnect()
	case "next":
		if len(args) == 0 {
			m.pushStatus("usage: /next <message> — send in the next turn")
			return m, nil
		}
		m.textarea.SetValue(strings.Join(args, " "))
		return m.submitInput(protocol.InputFollowup)
	case "pending":
		return m, m.openPendingDecision()
	case "tasks":
		m.tasksVisible = !m.tasksVisible
		return m, nil
	case "agents":
		return m, m.loadAgentCatalog(true)
	case "agent":
		return m, m.runAgentCommand(args)
	case "stop-tree":
		if m.routing == nil {
			return m, nil
		}
		directory, id, commandID := m.routing.directory, protocol.SessionID(m.routing.rootID), nextUICommandID()
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			run, err := directory.Control(ctx, protocol.AgentControl{ID: commandID, SessionID: id, Action: "stop_tree"})
			return agentControlMsg{id: id, run: run, err: err}
		}
	case "parent":
		return m, m.openAgentView(m.parentAgentID())
	case "root":
		return m, m.openAgentView("root")
	case "agent-history":
		var before uint64
		if len(args) > 0 {
			var err error
			before, err = strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				m.pushStatus("usage: /agent-history [before]")
				return m, nil
			}
		}
		return m, m.loadAgentHistory(before)
	case "agent-output":
		if len(args) == 0 {
			m.pushStatus("usage: /agent-output <item-id> [offset]")
			return m, nil
		}
		var offset int64
		if len(args) > 1 {
			var err error
			offset, err = strconv.ParseInt(args[1], 10, 64)
			if err != nil || offset < 0 {
				m.pushStatus("invalid output offset")
				return m, nil
			}
		}
		return m, m.loadAgentOutput(args[0], offset)
	case "transcript":
		if len(args) != 0 {
			m.pushLog("error", "usage: /transcript (open the full read-only transcript; /agent-output reads one saved output)")
			return m, nil
		}
		return m, m.openTranscriptReader()
	case "help", "?":
		m.pushLog("system", commandCatalog.Help())

	case "clear":
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandClearConversation}, "history clear submitted")

	case "compact":
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandCompact}, "compacting…")

	case "cost":
		m.pushLog("system", renderUsage(m.usage, m.modelName))

	case "context":
		if len(args) != 0 {
			m.pushLog("error", "usage: /context")
			return m, nil
		}
		m.pushLog("system", m.renderContext())
		m.showContextDetail = true
		return m, nil

	case "copy":
		if len(args) > 1 {
			m.pushLog("error", "usage: /copy [message-id] (copies the latest response by default)")
			return m, nil
		}
		if len(args) == 1 {
			return m, m.copyLatestResponse(args[0])
		}
		return m, m.copyLatestResponse()

	case "plugins":
		return m, m.runQuery(protocol.QueryPlugins, "plugin report requested")

	case "mcp":
		return m, m.runQuery(protocol.QueryMCP, "MCP report requested")

	case "init":
		if len(args) != 0 {
			m.pushLog("error", "usage: /init")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandInit, Init: &protocol.InitCommand{}}, "project instructions request submitted")

	case "mode":
		if len(args) == 0 {
			modes := append([]permissions.Mode(nil), permissions.ValidModes...)
			descriptions := map[permissions.Mode]string{
				permissions.ModeDefault:     "ask before risky operations",
				permissions.ModeAcceptEdits: "allow file edits; ask for other risky operations",
				permissions.ModePlan:        "plan without changing files",
				permissions.ModeBypass:      "allow operations without approval",
			}
			lines := make([]string, len(modes))
			selected := 0
			for i, mode := range modes {
				lines[i] = fmt.Sprintf("%s — %s", mode.Label(), descriptions[mode])
				if mode == m.mode {
					selected = i
				}
			}
			options := make([]SelectorOption, len(modes))
			for i, mode := range modes {
				options[i] = SelectorOption{ID: string(mode), Label: mode.Label(), Description: descriptions[mode],
					Current: mode == m.mode}
			}
			return m, m.startSelectorAt("Select permission mode", options, selected, true, selectorAction{Kind: selectorMode})
		}
		mode, err := permissions.ParseMode(args[0])
		if err != nil {
			m.pushLog("error", err.Error())
			return m, nil
		}
		policy := m.permissionPolicy()
		policy.Mode = string(mode)
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetPermissionPolicy,
			PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}, "permission mode → "+mode.Label())

	case "effort", "verbosity":
		if len(args) == 0 {
			return m, m.startGenerationSelector(cmd)
		}
		if len(args) != 1 {
			m.pushLog("error", "usage: /"+cmd+" <value|default>")
			return m, nil
		}
		value := args[0]
		if value == "default" {
			value = ""
		}
		g := &protocol.SetGeneration{}
		if cmd == "effort" {
			g.ReasoningEffort = &value
		} else {
			g.Verbosity = &value
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetGeneration, Generation: g}, cmd+" updated")
	case "model":
		if len(args) == 0 {
			models := m.availableModels()
			selected := 0
			currentModel := m.modelName
			if m.hasSnapshot && m.snapshot.Settings.Model.Model != "" {
				currentModel = m.snapshot.Settings.Model.Model
			}
			for i, model := range models {
				if model == currentModel {
					selected = i
					break
				}
			}
			options := make([]SelectorOption, len(models))
			for i, model := range models {
				options[i] = SelectorOption{ID: model, Label: model, Current: model == currentModel}
			}
			return m, m.startSelectorAt("Select model", options, selected, true, selectorAction{Kind: selectorModel})
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetModel,
			Model: &protocol.SetModel{Model: args[0]}}, "model → "+args[0])

	case "plan":
		on := !m.planMode
		if m.hasSnapshot {
			on = m.snapshot.Settings.ExecutionMode != protocol.ExecutionModePlan
		}
		if len(args) > 0 {
			switch args[0] {
			case "on":
				on = true
			case "off":
				on = false
			default:
				m.pushLog("error", "usage: /plan [on|off]")
				return m, nil
			}
		}
		mode := protocol.ExecutionModeExecute
		if on {
			mode = protocol.ExecutionModePlan
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetExecutionMode,
			ExecutionMode: &protocol.SetExecutionMode{Mode: mode}}, map[bool]string{true: "plan mode on", false: "plan mode off"}[on])

	case "sandbox":
		if len(args) == 0 {
			modes := append([]sandbox.Mode(nil), sandbox.ValidModes...)
			descriptions := map[sandbox.Mode]string{
				sandbox.ModeConfine: "confine writes to the workspace",
				sandbox.ModeStrict:  "confine all file access to the workspace",
				sandbox.ModeNone:    "disable containment",
			}
			current := sandbox.Mode(m.sandboxPolicy().Mode)
			lines := make([]string, len(modes))
			selected := 0
			for i, mode := range modes {
				lines[i] = fmt.Sprintf("%s — %s", string(mode), descriptions[mode])
				if mode == current {
					selected = i
				}
			}
			options := make([]SelectorOption, len(modes))
			for i, mode := range modes {
				options[i] = SelectorOption{ID: string(mode), Label: string(mode), Description: descriptions[mode],
					Current: mode == current}
			}
			return m, m.startSelectorAt("Select sandbox mode", options, selected, true, selectorAction{Kind: selectorSandbox})
		}
		mode, err := sandbox.ParseMode(args[0])
		if err != nil {
			m.pushLog("error", err.Error())
			return m, nil
		}
		policy := m.sandboxPolicy()
		policy.Mode = string(mode)
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetSandboxPolicy,
			SandboxPolicy: &protocol.SetSandboxPolicy{Policy: policy}}, "sandbox mode → "+string(mode))

	case "remove":
		n := 1
		if len(args) > 0 {
			v, err := strconv.Atoi(args[0])
			if err != nil || v < 1 {
				m.pushLog("error", "usage: /remove [n] (n >= 1)")
				return m, nil
			}
			n = v
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandRemoveMessages,
			Remove: &protocol.RemoveMessages{Count: n}}, fmt.Sprintf("removing last %d message(s)…", n))

	case "rewind":
		// No argument → interactive picker over the recent messages.
		if len(args) == 0 {
			total, previews := m.historyPreview()
			if len(previews) == 0 {
				m.pushLog("error", "nothing to rewind to")
				return m, nil
			}
			lines := make([]string, len(previews))
			for i, p := range previews {
				lines[i] = p
			}
			options := make([]SelectorOption, len(lines))
			for i, line := range lines {
				keep := total - len(lines) + i + 1
				if keep < 0 {
					keep = 0
				}
				options[i] = SelectorOption{ID: "keep:" + strconv.Itoa(keep), Label: line}
			}
			return m, m.startSelectorAt("Rewind to message", options, len(options)-1, true, selectorAction{Kind: selectorRewind})
		}
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 {
			m.pushLog("error", "usage: /rewind [<n>] (keep the first n messages; no arg picks interactively)")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandRewindConversation,
			Rewind: &protocol.RewindConversation{Count: n}}, fmt.Sprintf("rewinding to message %d…", n))

	case "fork":
		// /fork [n] — branch a new session from message n (pi's session tree).
		// No argument forks the whole history; the abandoned direction is
		// summarized by the model into the new session.
		n := -1
		if len(args) > 0 {
			v, err := strconv.Atoi(args[0])
			if err != nil || v < 0 {
				m.pushLog("error", "usage: /fork [n] (branch from message n; no arg = full copy)")
				return m, nil
			}
			n = v
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandFork,
			Fork: &protocol.Fork{Count: n}}, "forking session…")

	case "checkpoint":
		action := protocol.CheckpointList
		var id, summary string
		switch {
		case len(args) == 0, len(args) == 1 && strings.EqualFold(args[0], "list"):
			action = protocol.CheckpointList
		case strings.EqualFold(args[0], "create"):
			action = protocol.CheckpointCreate
			summary = strings.Join(args[1:], " ")
		case strings.EqualFold(args[0], "restore") && len(args) == 2:
			action, id = protocol.CheckpointRestore, args[1]
		default:
			m.pushLog("error", "usage: /checkpoint [list|create [summary]|restore <id>")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandCheckpoint,
			Checkpoint: &protocol.CheckpointCommand{Action: action, ID: id, Summary: summary}}, "checkpoint request submitted")

	case "review":
		m.pushStatus("loading review…")
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandRunWorkflow,
			Workflow: &protocol.WorkflowCommand{Kind: protocol.WorkflowReview}}, "review workflow submitted")

	case "add-dir":
		if len(args) == 0 {
			m.pushLog("error", "usage: /add-dir <directory>")
			return m, nil
		}
		policy := m.sandboxPolicy()
		if !containsString(policy.AdditionalDirectories, args[0]) {
			policy.AdditionalDirectories = append(policy.AdditionalDirectories, args[0])
		}
		if m.client == nil {
			m.pushLog("error", "/add-dir requires a session protocol")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetSandboxPolicy,
			SandboxPolicy: &protocol.SetSandboxPolicy{Policy: policy}}, "additional directory change submitted")

	case "disallowed-dir":
		if len(args) == 0 {
			m.pushLog("error", "usage: /disallowed-dir <directory>")
			return m, nil
		}
		policy := m.sandboxPolicy()
		if !containsString(policy.DisallowedDirectories, args[0]) {
			policy.DisallowedDirectories = append(policy.DisallowedDirectories, args[0])
		}
		if m.client == nil {
			m.pushLog("error", "/disallowed-dir requires a session protocol")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetSandboxPolicy,
			SandboxPolicy: &protocol.SetSandboxPolicy{Policy: policy}}, "disallowed directory change submitted")

	case "export":
		if len(args) > 1 {
			m.pushLog("error", "usage: /export [path]")
			return m, nil
		}
		path := ""
		if len(args) > 0 {
			path = args[0]
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandExport,
			Export: &protocol.ExportCommand{Path: path}}, "export request submitted")

	case "diff":
		m.pushStatus("loading diff…")
		return m, m.runDiff(args)

	case "cd":
		if len(args) != 1 {
			m.pushLog("error", "usage: /cd <dir>")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetWorkspace,
			Workspace: &protocol.WorkspaceCommand{Path: args[0]}}, "workspace change submitted")

	case "pwd":
		m.pushStatus("workspace: " + m.workspace)

	case "statusline":
		if len(args) == 0 {
			m.pushStatus("statusline extras: " + strings.Join(m.statusItems, " ") + " · model effort context always visible")
			return m, nil
		}
		if args[0] == "default" {
			m.statusItems = append([]string{}, defaultStatusItems...)
			m.pushStatus("statusline extras reset · model effort context always visible")
			return m, nil
		}
		var items []string
		for _, a := range args {
			if !statuslineTokens[a] {
				m.pushLog("error", fmt.Sprintf("unknown statusline item %q (valid: version model mode session workspace cost context effort)", a))
				return m, nil
			}
			items = append(items, a)
		}
		m.statusItems = items
		m.pushStatus("statusline extras: " + strings.Join(items, " ") + " · model effort context always visible")

	case "skills":
		return m, m.runQuery(protocol.QuerySkills, "skills report requested")

	case "apply":
		if len(args) == 0 {
			m.pushLog("error", "usage: /apply <file.md|file.patch> — applies a plan or git patch to the workspace")
			return m, nil
		}
		return m, m.runApply(args[0])

	case "git":
		if len(args) == 0 {
			m.pushLog("error", "usage: /git <subcommand> [args…], e.g. /git status --short")
			return m, nil
		}
		m.pushStatus("running git…")
		return m, m.runGit(args)

	case "save":
		if len(args) != 0 {
			m.pushLog("error", "usage: /save")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandSaveSession,
			SaveSession: &protocol.SaveSessionCommand{}}, "session save submitted")

	case "sessions":
		if m.ag == nil {
			m.pushLog("error", "/sessions is unavailable through this session protocol")
			return m, nil
		}
		sessions, issues, err := m.savedSessions()
		if err != nil {
			m.pushLog("error", err.Error())
			return m, nil
		}
		if len(sessions) == 0 {
			if note := sessionListIssuesNote(issues); note != "" {
				m.pushLog("system", "no saved sessions could be read\n"+note)
			} else {
				m.pushLog("system", "no saved sessions")
			}
			return m, nil
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
			forkNote := ""
			if s.ParentID != "" {
				forkNote = fmt.Sprintf("  ← fork of %s @ %d", s.ParentID, s.BranchPoint)
			}
			fmt.Fprintf(&sb, "  %s  %s  %s\n    %s  (%d messages)%s\n",
				s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), s.Model, title, len(s.History), forkNote)
		}
		if note := sessionListIssuesNote(issues); note != "" {
			sb.WriteString("\n" + note + "\n")
		}
		m.pushLog("system", sb.String())

	case "resume":
		// No argument → interactive picker over saved sessions.
		if len(args) == 0 {
			return m, m.startResumePicker()
		}
		return m, m.openResumeSession(args[0])

	case "config":
		if len(args) == 0 {
			return m, m.runQuery(protocol.QueryConfig, "configuration report requested")
		}
		// /config set <key> <value> — runtime settable keys reuse the existing
		// commands (model, mode, sandbox); anything else is read-only.
		if args[0] == "set" && len(args) >= 3 {
			key, val := args[1], strings.Join(args[2:], " ")
			switch key {
			case "effort", "verbosity":
				return m.runCommand("/" + key + " " + val)
			case "model":
				return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetModel,
					Model: &protocol.SetModel{Model: val}}, "model change submitted")
			case "mode":
				mode, err := permissions.ParseMode(val)
				if err != nil {
					m.pushLog("error", err.Error())
					return m, nil
				}
				policy := m.permissionPolicy()
				policy.Mode = string(mode)
				return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetPermissionPolicy,
					PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}, "permission mode change submitted")
			case "sandbox":
				mode, err := sandbox.ParseMode(val)
				if err != nil {
					m.pushLog("error", err.Error())
					return m, nil
				}
				policy := m.sandboxPolicy()
				policy.Mode = string(mode)
				return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetSandboxPolicy,
					SandboxPolicy: &protocol.SetSandboxPolicy{Policy: policy}}, "sandbox mode change submitted")
			default:
				m.pushLog("error", "/config set supports: model, mode, sandbox (others are read-only)")
			}
			return m, nil
		}
		m.pushLog("error", "usage: /config  |  /config set <model|mode|sandbox> <value>")

	case "doctor":
		return m, m.runQuery(protocol.QueryDoctor, "diagnostics report requested")

	case "permissions":
		if len(args) == 0 {
			return m, m.runQuery(protocol.QueryPermissions, "permission report requested")
		}
		// /permissions allow|deny|remove <rule>
		switch args[0] {
		case "allow", "deny":
			if len(args) < 2 {
				m.pushLog("error", "usage: /permissions allow|deny <rule> (e.g. Bash:git status)")
				return m, nil
			}
			rule := strings.Join(args[1:], " ")
			if err := permissions.ValidatePermissionRule(args[0], rule); err != nil {
				m.pushLog("error", err.Error())
				return m, nil
			}
			if m.client == nil {
				m.pushLog("error", "/permissions requires a session protocol")
				return m, nil
			}
			policy := m.permissionPolicy()
			if args[0] == "allow" {
				if !containsString(policy.AlwaysAllow, rule) {
					policy.AlwaysAllow = append(policy.AlwaysAllow, rule)
				}
			} else if !containsString(policy.AlwaysDeny, rule) {
				policy.AlwaysDeny = append(policy.AlwaysDeny, rule)
			}
			return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetPermissionPolicy,
				PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}, "permission rule change submitted")
		case "remove":
			if len(args) < 3 {
				m.pushLog("error", "usage: /permissions remove <allow|deny> <rule>")
				return m, nil
			}
			rule := strings.Join(args[2:], " ")
			if m.client == nil {
				m.pushLog("error", "/permissions requires a session protocol")
				return m, nil
			}
			policy := m.permissionPolicy()
			if args[1] == "allow" {
				policy.AlwaysAllow = removeString(policy.AlwaysAllow, rule)
			} else if args[1] == "deny" {
				policy.AlwaysDeny = removeString(policy.AlwaysDeny, rule)
			} else {
				m.pushLog("error", "usage: /permissions remove <allow|deny> <rule>")
				return m, nil
			}
			return m, m.submitCommand(protocol.Command{Type: protocol.CommandSetPermissionPolicy,
				PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}, "permission rule change submitted")
		default:
			m.pushLog("error", "usage: /permissions [allow|deny|remove <rule>]")
		}

	case "memory":
		if len(args) == 0 {
			return m, m.runQuery(protocol.QueryMemory, "memory report requested")
		}
		if len(args) == 1 && strings.EqualFold(args[0], "clear") {
			return m, m.submitCommand(protocol.Command{Type: protocol.CommandClearMemory,
				ClearMemory: &protocol.ClearMemoryCommand{}}, "memory clear submitted")
		}
		m.pushLog("error", "usage: /memory [clear]")
		return m, nil

	case "github":
		if len(args) != 0 {
			m.pushLog("error", "usage: /github")
			return m, nil
		}
		return m, m.runQuery(protocol.QueryGitHub, "GitHub status requested")

	case "pr-comments":
		if len(args) != 0 {
			m.pushLog("error", "usage: /pr-comments")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandExternal,
			External: &protocol.ExternalCommand{Program: "gh", Args: []string{"pr", "view", "--comments"}}}, "PR comments requested")

	case "commit-push-pr":
		msg := strings.Join(args, " ")
		if msg == "" {
			m.pushLog("error", "usage: /commit-push-pr <commit message>")
			return m, nil
		}
		m.pushStatus("committing, pushing and opening PR…")
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandRunWorkflow,
			Workflow: &protocol.WorkflowCommand{Kind: protocol.WorkflowCommitPushPR, Message: msg}}, "commit/push/PR workflow submitted")

	case "reload":
		if len(args) != 0 {
			m.pushLog("error", "usage: /reload")
			return m, nil
		}
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandReloadSettings,
			Reload: &protocol.ReloadCommand{}}, "settings reload submitted")

	case "status":
		if len(args) != 0 {
			m.pushLog("error", "usage: /status")
			return m, nil
		}
		return m, m.runQuery(protocol.QueryStatus, "status report requested")

	case "trust":
		if len(args) > 1 || (len(args) == 1 && !strings.EqualFold(args[0], "revoke") && !strings.EqualFold(args[0], "untrust")) {
			m.pushLog("error", "usage: /trust [revoke]")
			return m, nil
		}
		revoke := len(args) == 1
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandTrustProject,
			TrustProject: &protocol.TrustProjectCommand{Revoke: revoke}}, "project trust change submitted")

	case "commands":
		names := m.customCommandNames()
		if len(names) == 0 {
			m.pushLog("system", "no custom commands — create ~/.ccdp/commands/<name>.md or .ccdp/commands/<name>.md ($ARGUMENTS is replaced by the arguments)")
			return m, nil
		}
		m.pushLog("system", "custom commands: "+strings.Join(names, ", "))

	case "quit", "exit":
		return m, m.requestQuit()

	default:
		// User-defined slash commands (markdown templates).
		if cc := m.findCustomCommand(cmd); cc != nil {
			return m, m.runCustomCommand(cc, args)
		}
		m.pushLog("error", "unknown command /"+cmd+" (try /help)")
	}
	return m, nil
}

// runDiff requests a read-only git diff through the typed external-command
// protocol. Provider-side policy, sandboxing and execution remain authoritative
// for workspace data; the TUI never shells out directly.
func (m *Model) runDiff(paths []string) tea.Cmd {
	args := []string{"diff"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	return m.submitCommand(protocol.Command{Type: protocol.CommandExternal,
		External: &protocol.ExternalCommand{Program: "git", Args: args}}, "diff request submitted")
}

// runGit requests a user-selected workspace operation through the session
// protocol, preserving normal approval and sandbox policy.
func (m *Model) runGit(args []string) tea.Cmd {
	return m.submitCommand(protocol.Command{Type: protocol.CommandExternal,
		External: &protocol.ExternalCommand{Program: "git", Args: append([]string(nil), args...)}}, "git request submitted")
}

// startResumePicker opens the interactive saved-session selector.
func (m *Model) startResumePicker() tea.Cmd {
	if m.ag == nil {
		m.pushLog("error", "resume requires an agent session opener")
		return nil
	}
	sessions, issues, err := m.savedSessions()
	if err != nil {
		m.pushLog("error", err.Error())
		return nil
	}
	if len(sessions) == 0 {
		if note := sessionListIssuesNote(issues); note != "" {
			m.pushLog("system", "no saved sessions could be resumed\n"+note)
		} else {
			m.pushLog("system", "no saved sessions to resume")
		}
		return nil
	}
	lines := make([]string, 0, len(sessions))
	for _, s := range sessions {
		title := s.Title
		if title == "" {
			title = "(no user message)"
		}
		lines = append(lines, fmt.Sprintf("%s  %s\n    %s  (%d messages)",
			s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), title, len(s.History)))
	}
	options := make([]SelectorOption, len(sessions))
	for i, s := range sessions {
		options[i] = SelectorOption{ID: s.ID, Label: lines[i]}
	}
	return m.startSelectorAt("Resume a session", options, 0, true, selectorAction{Kind: selectorResume})
}

// runApply reads a bounded plan/patch file and submits its contents through the
// session protocol. A path alone never bypasses provider approval policy.
func (m *Model) runApply(path string) tea.Cmd {
	return m.submitCommand(protocol.Command{Type: protocol.CommandApply,
		Apply: &protocol.ApplyCommand{Path: path}}, "apply request submitted")
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
func renderUsage(u protocol.UsageSnapshot, model string) string {
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

// renderContext visualizes how much of the model's context window the session
// currently occupies. It uses only authoritative numbers: the runtime's live
// context estimate and window, plus cumulative request accounting. It never
// invents a per-section (system/tools/messages) split, which the runtime does
// not track, and it shows proportion as a bar rather than a printed percentage.
func (m *Model) renderContext() string {
	model, usage := m.modelName, m.usage
	window, used := 0, usage.InputTokens+usage.OutputTokens
	if m.hasSnapshot {
		model = m.snapshot.Settings.Model.Model
		usage = m.snapshot.Usage
		window = m.snapshot.Settings.ContextWindow
		used = m.snapshot.ContextUsedTokens
	}
	if used < 0 {
		used = 0
	}
	var b strings.Builder
	b.WriteString("Context (model " + model + "):\n")
	if window > 0 {
		free := max(0, window-used)
		b.WriteString(fmt.Sprintf("  window:  %s tokens\n", formatContextTokens(window)))
		b.WriteString(fmt.Sprintf("  in use:  %s tokens  %s\n", formatContextTokens(used), contextBar(used, window)))
		b.WriteString(fmt.Sprintf("  free:    %s tokens\n", formatContextTokens(free)))
	} else {
		b.WriteString("  window:  unknown (model reported no limit)\n")
		b.WriteString(fmt.Sprintf("  in use:  %s tokens\n", formatContextTokens(used)))
	}
	b.WriteString("\n  Cumulative this session:\n")
	b.WriteString(fmt.Sprintf("    input:   %d\n", usage.InputTokens))
	b.WriteString(fmt.Sprintf("    output:  %d\n", usage.OutputTokens))
	if usage.CachedTokens > 0 {
		b.WriteString(fmt.Sprintf("    cached:  %d\n", usage.CachedTokens))
	}
	b.WriteString(fmt.Sprintf("    turns:   %d\n", usage.TurnCount))
	b.WriteString(fmt.Sprintf("    cost:    $%.4f\n", usage.Cost))
	return b.String()
}

// contextBar draws a fixed-width proportion bar. Any non-zero use fills at
// least one cell so a nearly empty window is still visibly non-empty, and the
// bar saturates at the window rather than overflowing past it.
func contextBar(used, window int) string {
	if window <= 0 {
		return ""
	}
	const cells = 20
	filled := 0
	if used > 0 {
		filled = (used*cells + window - 1) / window
	}
	filled = min(cells, filled)
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", cells-filled) + "]"
}

// exportMarkdown serializes the retained protocol snapshot without reaching
// through the Agent adapter. Export is a client-side report, so it must remain
// stable across session swaps and cannot observe a later mutable runtime.
func exportMarkdown(snapshot protocol.SessionView, workspace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ccdp session %s\n\n", snapshot.SessionID)
	fmt.Fprintf(&b, "- Model: %s\n- Workspace: `%s`\n\n---\n\n",
		snapshot.Settings.Model.Model, workspace)
	for _, message := range snapshot.History {
		role := strings.ToLower(message.Role)
		switch role {
		case "user":
			fmt.Fprintf(&b, "## User\n\n%s\n\n", message.Content)
		case "assistant":
			fmt.Fprintf(&b, "## Assistant\n\n%s\n", message.Content)
			for _, callID := range message.ToolCallIDs {
				fmt.Fprintf(&b, "\n_→ tool(%s)_\n", callID)
			}
			b.WriteString("\n")
		case "tool":
			fmt.Fprintf(&b, "### Tool result\n\n```text\n%s\n```\n\n", message.Content)
		default:
			if message.Content != "" {
				fmt.Fprintf(&b, "### %s\n\n%s\n\n", message.Role, message.Content)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
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
	m.addReport(historyCell{kind: kind, text: text})
	m.render()
	m.followOutput = true
	m.viewport.GotoBottom()
}
