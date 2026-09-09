package agent

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/tools"
	"ccdp/internal/workspace"
)

// imageExts are the image formats we attach as multi-modal content parts.
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// markdownImageRe matches ![alt](path) image references.
var markdownImageRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

// imageContentParts turns a user message into multi-modal content parts when it
// references local images via markdown (![alt](path)). Returns nil when there
// are no images, in which case the caller sends the plain text.
func (a *Agent) imageContentParts(text string) []llm.ContentPart {
	matches := markdownImageRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil
	}
	var parts []llm.ContentPart
	last := 0
	for _, m := range matches {
		// Text between the previous marker and this one.
		if m[0] > last {
			parts = append(parts, llm.ContentPart{Type: "text", Text: text[last:m[0]]})
		}
		path := text[m[2]:m[3]]
		if part := a.imagePart(path); part != nil {
			parts = append(parts, *part)
		}
		last = m[1]
	}
	if last < len(text) {
		parts = append(parts, llm.ContentPart{Type: "text", Text: text[last:]})
	}
	// Only useful if at least one image actually loaded.
	for _, p := range parts {
		if p.ImageURL != nil {
			return parts
		}
	}
	return nil
}

// imagePart loads a local image into an image_url data part, or nil.
func (a *Agent) imagePart(path string) *llm.ContentPart {
	ext := strings.ToLower(filepath.Ext(path))
	if !imageExts[ext] {
		return nil
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(a.cfg.Workspace, abs)
	}
	data, err := os.ReadFile(abs)
	if err != nil || len(data) > 20*1024*1024 {
		return nil
	}
	mime := map[string]string{
		".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".gif": "image/gif", ".webp": "image/webp",
	}[ext]
	return &llm.ContentPart{
		Type: "image_url",
		ImageURL: &struct {
			URL string `json:"url"`
		}{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)},
	}
}

// planReadOnlyTools is the tool set the model may call while in plan mode.
// Everything that can mutate state (files, shell side effects, git history,
// network) is excluded until the plan is approved.
var planReadOnlyTools = map[string]bool{
	"Read": true, "Glob": true, "Grep": true, "LS": true,
	"TodoWrite": true, "WebSearch": true, "WebFetch": true,
	"GitStatus": true, "GitDiff": true, "GitLog": true,
	"Task": true, "ToolSearch": true, "ReadSkill": true,
	"EnterPlanMode": true, "ExitPlanMode": true,
}

// PlanModeInstructions is appended to the system prompt while plan mode is on.
const PlanModeInstructions = `
You are in PLAN MODE. Do NOT execute anything yet.
- Analyze the user's request and produce a concise, concrete execution plan.
- Structure the plan as a numbered list of steps; name the exact files, commands
  and tools each step will use.
- You may call read-only tools (Read, Glob, Grep, LS, TodoWrite, WebSearch,
  WebFetch, GitStatus, GitDiff, GitLog, Task) to investigate before planning.
- Do NOT call any tool that modifies files, runs builds or tests with side
  effects, touches git history, or makes network calls.
- End your reply with the plan only. The user will approve it before execution.`

// buildRequest assembles the API request from the current history, the tool
// schemas and the system prompt. The system prompt is a stable base (cached by
// providers) followed by per-turn dynamic sections, mirroring Claude Code's
// static + dynamic sections and Codex's environment context.
func (a *Agent) buildRequest() llm.CompletionRequest {
	sys := a.cfg.SystemPrompt

	// Environment + session context (Codex's environment_context idea). The
	// date is fixed per session so the prompt prefix stays cache-stable.
	sys += fmt.Sprintf("\n\n# Environment\n- Date: %s\n- Timezone: %s\n- OS: %s\n- Shell: %s",
		a.sessionStartAt.Format("2006-01-02 15:04:05"), time.Local.String(), runtime.GOOS, shellName())
	sys += fmt.Sprintf("\n- Workspace: %s\n- Permission mode: %s\n- Session: %s",
		a.cfg.Workspace, a.perms.CurrentMode(), a.sessionID)

	// Model switch instruction (Codex's ModelSwitchInstructions) — injected
	// once, then cleared.
	a.mu.Lock()
	if a.modelSwitchMsg != "" {
		sys += "\n\n" + a.modelSwitchMsg
		a.modelSwitchMsg = ""
	}
	// Interrupted-turn guidance (Codex's INTERRUPTED_GUIDANCE): injected on
	// the first request after a user interrupt, then cleared.
	if a.interruptNote {
		sys += "\n\n# Interrupted turn\nYour previous turn was interrupted by the user. Tools that were\nrunning may have partially executed and their effects may be incomplete.\nBefore continuing, verify the relevant state (re-read files, re-run git\nstatus or tests) rather than assuming the last known state."
		a.interruptNote = false
	}
	a.mu.Unlock()

	if a.inPlanMode() {
		sys += "\n\n" + PlanModeInstructions
	}

	// Project instructions (AGENTS.md): user-level, git root, workspace.
	if instructions := workspace.LoadInstructions(a.cfg.Workspace); instructions != "" {
		sys += "\n\n# Project instructions\n\n" + instructions
	}

	// Session memory (AutoMem): facts learned earlier in this session. Injected
	// only when enable_memory is on and the log is non-empty.
	if a.cfg.EnableMemory {
		if sec := a.MemorySection(); sec != "" {
			sys += sec
		}
	}

	// Repo map: a compact file inventory so the model can plan tool calls.
	if info, err := workspace.CachedScan(a.cfg.Workspace, false); err == nil && info != nil {
		a.wsInfo = info
		sys += "\n\n# Repository layout\n\n" + info.RepoMap(120)
	}

	// Skill index (name + description only; bodies load via ReadSkill).
	if sec := a.skills.SkillsSection(); sec != "" {
		sys += sec
	}

	// Active task list (Claude Code's scratchpad) — stable per session.
	if sec := tools.TodoSection(a.cfg.SessionDir); sec != "" {
		sys += "\n\n" + sec
	}

	// Dynamic tool availability (never hardcodes the tool list).
	sys += "\n\n" + a.availableTools()

	req := a.buildRequestFrom(sys, a.history)
	if a.inPlanMode() {
		req.Tools = filterReadOnlyTools(req.Tools)
	}
	return req
}

// shellName returns the user's shell for the environment section.
func shellName() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

// filterReadOnlyTools narrows a tool list to the plan-mode safe subset.
func filterReadOnlyTools(tools []llm.ToolDef) []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(tools))
	for _, t := range tools {
		if planReadOnlyTools[t.Function.Name] {
			out = append(out, t)
		}
	}
	return out
}

// buildRequestFrom assembles an API request from a caller-provided system
// prompt and history. The main loop and sub-agent loops share this path so the
// wire format stays identical everywhere.
func (a *Agent) buildRequestFrom(sys string, history []messages.Message) llm.CompletionRequest {
	msgs := make([]llm.ChatMessage, 0, len(history)+1)
	msgs = append(msgs, llm.ChatMessage{Role: "system", Content: sys})

	for _, m := range history {
		switch m.Role {
		case messages.RoleSystem:
			msgs = append(msgs, llm.ChatMessage{Role: "system", Content: m.Content})
		case messages.RoleUser:
			// Multi-modal: attach local images referenced as ![alt](path).
			if parts := a.imageContentParts(m.Content); len(parts) > 0 {
				msgs = append(msgs, llm.ChatMessage{Role: "user", Content: parts})
				continue
			}
			msgs = append(msgs, llm.ChatMessage{Role: "user", Content: m.Content})
		case messages.RoleAssistant:
			cm := llm.ChatMessage{Role: "assistant", Content: m.Content}
			if len(m.ToolCalls) > 0 {
				cm.ToolCalls = wireToolCalls(m.ToolCalls)
			}
			msgs = append(msgs, cm)
		case messages.RoleTool:
			msgs = append(msgs, llm.ChatMessage{
				Role:       "tool",
				ToolCallID: m.ToolCallID,
				Content:    m.Content,
			})
		}
	}

	// Tool schemas (Codex: inject every turn; cheap with prompt caching).
	// Deferred tools are excluded until the model discovers them via ToolSearch.
	schemas := a.toolSchemas()
	tools := make([]llm.ToolDef, 0, len(schemas))
	for _, s := range schemas {
		tools = append(tools, llm.ToolDef{
			Type: "function",
			Function: llm.FuncDef{
				Name:        s["function"].(map[string]any)["name"].(string),
				Description: s["function"].(map[string]any)["description"].(string),
				Parameters:  s["function"].(map[string]any)["parameters"].(map[string]any),
			},
		})
	}

	req := llm.CompletionRequest{
		Model:    a.cfg.Model,
		Messages: msgs,
		Tools:    tools,
		Stream:   true,
	}
	if a.cfg.MaxReplyTokens > 0 {
		req.MaxTokens = intPtr(a.cfg.MaxReplyTokens)
	}
	return req
}

func wireToolCalls(calls []messages.ToolCall) []llm.ToolCall {
	out := make([]llm.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, llm.ToolCall{
			ID:       c.ID,
			Type:     "function",
			Function: llm.Function{Name: c.Name, Arguments: llm.ArgumentsJSON(messages.MarshalArguments(c.Arguments))},
		})
	}
	return out
}

// truncateResult caps a tool result at maxChars, appending a marker when
// content was cut so the model knows the result was truncated.
func truncateResult(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	cut := s[:maxChars]
	return cut + fmt.Sprintf("\n…[output truncated at %d chars, %d more available]",
		maxChars, len(s)-maxChars)
}

// describeHistory returns a human-readable outline of the conversation
// (used by the TUI header / session info).
func (a *Agent) describeHistory() string {
	var parts []string
	for _, m := range a.history {
		preview := strings.ReplaceAll(m.Content, "\n", " ")
		if len(preview) > 40 {
			preview = preview[:40] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", m.Role, preview))
	}
	return strings.Join(parts, "\n")
}
