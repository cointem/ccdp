package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/sandbox"
	"ccdp/internal/workspace"
)

// PlanModeInstructions is appended to the system prompt while plan mode is on.
const PlanModeInstructions = `
You are in PLAN MODE. Do NOT execute anything yet.
- Analyze the user's request and produce a concise, concrete execution plan.
- Structure the plan as a numbered list of steps; name the exact files, commands
  and tools each step will use.
- You may call read-only tools (Read, Glob, Rg, LS, TodoWrite,
  CodeNavigate, ListAgents, ReadAgent, WaitAgent) to investigate before planning.
- Do NOT call any tool that modifies files, runs builds or tests with side
  effects, touches git history, or makes network calls.
- Use AskUserQuestion for structured clarification when requirements are ambiguous.
- End your reply with the plan only. The user will approve it before execution.`

// buildSystemBlocks composes the system prompt as an ordered list of sections.
// Cacheable sections (core instructions, tools, skills index, project
// instructions) lead the list; a wire that
// supports prompt caching (Anthropic cache_control) can hold them across
// requests. Query-dependent repository layout and other dynamic sections
// (environment, memory, plan, todo) follow the stable prefix.
func (a *Agent) buildSystemBlocks(cfg config.Config, sessionStartAt time.Time, sessionID string, plan bool, modelSwitch string, interrupted bool, instructions, skillsSection string, frozenTools []llm.ToolDef, history []messages.Message, policy *sandbox.Sandbox) []llm.SystemBlock {
	blocks := make([]llm.SystemBlock, 0, 11)
	add := func(cacheable bool, text string) {
		if text == "" {
			return
		}
		blocks = append(blocks, llm.SystemBlock{Text: text, Cacheable: cacheable})
	}

	// Stable, cacheable prefix.
	add(true, cfg.SystemPrompt)
	add(true, availableToolsFromDefs(frozenTools))
	add(true, skillsSection)
	if instructions != "" {
		add(true, "\n\n# Project instructions\n\n"+instructions)
	}

	// Dynamic tail past the cache boundary.
	if info, err := workspace.CachedScan(cfg.Workspace, false); err == nil && info != nil {
		query := ""
		for n := len(history) - 1; n >= 0; n-- {
			if history[n].Role == messages.RoleUser {
				query = history[n].Content
				break
			}
		}
		filtered := *info
		filtered.Files = nil
		filtered.FileCount = 0
		for _, entry := range info.Files {
			if _, err := policy.ResolveRead(filepath.Join(cfg.Workspace, entry.RelPath)); err == nil {
				filtered.Files = append(filtered.Files, entry)
				if !entry.IsDir {
					filtered.FileCount++
				}
			}
		}
		add(false, "\n\n# Repository layout\n\n"+filtered.RelevantRepoMap(120, query))
	}
	add(false, fmt.Sprintf("\n\n# Environment\n- Date: %s\n- Timezone: %s\n- OS: %s\n- Shell: %s\n- Workspace: %s\n- Permission mode: %s\n- Session: %s",
		sessionStartAt.Format("2006-01-02 15:04:05"), time.Local.String(), runtime.GOOS, shellName(),
		cfg.Workspace, cfg.PermissionMode, sessionID))
	if modelSwitch != "" {
		add(false, "\n\n"+modelSwitch)
	}
	if interrupted {
		add(false, "\n\n# Interrupted turn\nYour previous turn was interrupted by the user. Tools that were\nrunning may have partially executed and their effects may be incomplete.\nBefore continuing, verify the relevant state (re-read files, re-run git\nstatus or tests) rather than assuming the last known state.")
	}
	if plan {
		add(false, "\n\n"+PlanModeInstructions)
	}
	if cfg.MemoryEnabled() {
		if sec := a.MemorySection(); sec != "" {
			add(false, sec)
		}
	}
	if sec := a.todoSection(); sec != "" {
		add(false, "\n\n"+sec)
	}
	return blocks
}

// joinSystemBlocks concatenates the ordered system sections into the single
// system string used by the chat/responses wires.
func joinSystemBlocks(blocks []llm.SystemBlock) string {
	var sb strings.Builder
	for i, b := range blocks {
		if i > 0 && !strings.HasPrefix(b.Text, "\n") {
			sb.WriteString("\n\n")
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}

func availableToolsFromDefs(defs []llm.ToolDef) string {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		if name := strings.TrimSpace(def.Function.Name); name != "" {
			names = append(names, name)
		}
	}
	var sb strings.Builder
	sb.WriteString("# Available tools\n")
	if len(names) > 0 {
		sb.WriteString("Available directly: " + strings.Join(names, ", ") + "\n")
	}
	return sb.String()
}

func (a *Agent) todoSection() string {
	text, _ := a.todoSectionChecked()
	return text
}

func (a *Agent) todoSectionChecked() (string, error) {
	a.mu.Lock()
	resources := a.services.resources
	a.mu.Unlock()
	if resources == nil || resources.Todos == nil {
		return "", nil
	}
	return resources.Todos.Section()
}

// shellName returns the user's shell for the environment section.
func shellName() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}
