package agent

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/session"
	"ccdp/internal/workspace"
)

// imageExts are the image formats we attach as multi-modal content parts.
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// markdownImageRe matches ![alt](path) image references.
var markdownImageRe = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

const (
	maxImageBytes      = 20 * 1024 * 1024
	maxAttachmentBytes = 40 * 1024 * 1024
)

// imageContentParts turns a user message into multi-modal content parts when it
// references local images via markdown (![alt](path)). Returns nil when there
// are no images, in which case the caller sends the plain text.
func (a *Agent) imageContentParts(text string) []llm.ContentPart {
	a.mu.Lock()
	workspaceDir := a.cfg.Workspace
	a.mu.Unlock()
	return imageContentPartsAt(workspaceDir, text)
}

func imageContentPartsAt(workspaceDir, text string) []llm.ContentPart {
	parts, _, hasImage, _ := imageContentPartsAtBudget(workspaceDir, text, maxAttachmentBytes)
	if !hasImage {
		return nil
	}
	return parts
}

// freezeImageAttachmentsAt captures each readable local image exactly once at
// input admission. Failed references remain ordinary markdown and are not
// retried from a later, possibly different file version.
func freezeImageAttachmentsAt(workspaceDir, text string) ([]messages.ImageAttachment, error) {
	matches := markdownImageRe.FindAllStringSubmatchIndex(text, -1)
	attachments := make([]messages.ImageAttachment, 0, len(matches))
	var used int64
	for _, match := range matches {
		path := strings.TrimSpace(text[match[2]:match[3]])
		data, mediaType, err := imageBytesAtBudget(workspaceDir, path, maxAttachmentBytes-used)
		if err != nil {
			return nil, fmt.Errorf("capture image %q: %w", path, err)
		}
		attachments = append(attachments, messages.ImageAttachment{Path: path, MediaType: mediaType, Data: data})
		used += int64(len(data))
	}
	return attachments, nil
}

func (a *Agent) freezeImageAttachments(text string) ([]messages.ImageAttachment, error) {
	a.mu.Lock()
	workspaceDir := a.cfg.Workspace
	a.mu.Unlock()
	return freezeImageAttachmentsAt(workspaceDir, text)
}

// imageContentPartsAtBudget expands local image references while preserving
// failed references as ordinary text. The byte budget is checked from stat
// metadata before reading and io.LimitReader caps the allocation if a file
// grows between stat and read. used is the number of bytes actually attached.
func imageContentPartsAtBudget(workspaceDir, text string, budget int64) (parts []llm.ContentPart, used int64, hasImage bool, firstErr error) {
	matches := markdownImageRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, 0, false, nil
	}
	last := 0
	for _, m := range matches {
		// Text between the previous marker and this one.
		if m[0] > last {
			parts = append(parts, llm.ContentPart{Type: "text", Text: text[last:m[0]]})
		}
		marker := text[m[0]:m[1]]
		path := strings.TrimSpace(text[m[2]:m[3]])
		part, size, err := imagePartAtBudget(workspaceDir, path, budget-used)
		if err == nil && part != nil {
			parts = append(parts, *part)
			used += size
			hasImage = true
		} else {
			// Keep the original markdown in the model-visible input. A missing,
			// oversized, or unreadable image must never disappear silently.
			parts = append(parts, llm.ContentPart{Type: "text", Text: marker})
			if firstErr == nil {
				firstErr = err
			}
		}
		last = m[1]
	}
	if last < len(text) {
		parts = append(parts, llm.ContentPart{Type: "text", Text: text[last:]})
	}
	return parts, used, hasImage, firstErr
}

// imagePart loads a local image into an image_url data part, or nil.
func (a *Agent) imagePart(path string) *llm.ContentPart {
	a.mu.Lock()
	workspaceDir := a.cfg.Workspace
	a.mu.Unlock()
	return imagePartAt(workspaceDir, path)
}

func imagePartAt(workspaceDir, path string) *llm.ContentPart {
	part, _, err := imagePartAtBudget(workspaceDir, path, maxImageBytes)
	if err != nil {
		return nil
	}
	return part
}

func imagePartAtBudget(workspaceDir, path string, budget int64) (*llm.ContentPart, int64, error) {
	data, mime, err := imageBytesAtBudget(workspaceDir, path, budget)
	if err != nil {
		return nil, 0, err
	}
	return &llm.ContentPart{
		Type: "image_url",
		ImageURL: &struct {
			URL string `json:"url"`
		}{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)},
	}, int64(len(data)), nil
}

func imageBytesAtBudget(workspaceDir, path string, budget int64) ([]byte, string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if !imageExts[ext] {
		return nil, "", fmt.Errorf("unsupported image extension %q", ext)
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(workspaceDir, abs)
	}
	root, err := filepath.Abs(workspaceDir)
	if err != nil {
		return nil, "", err
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("image path escapes workspace")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", err
	}
	rel, err = filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("image path escapes workspace")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("image %q is not a regular file", path)
	}
	if info.Size() > maxImageBytes {
		return nil, "", fmt.Errorf("image %q exceeds per-file limit", path)
	}
	if budget <= 0 || info.Size() > budget {
		return nil, "", fmt.Errorf("image %q exceeds total attachment limit", path)
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	readLimit := int64(maxImageBytes)
	if budget < readLimit {
		readLimit = budget
	}
	// Include one sentinel byte so a file that grows after Stat is rejected,
	// while never allocating beyond the remaining total attachment budget.
	data, err := io.ReadAll(io.LimitReader(file, readLimit+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("image %q exceeds per-file limit", path)
	}
	if int64(len(data)) > budget {
		return nil, "", fmt.Errorf("image %q exceeds total attachment limit", path)
	}
	mime := map[string]string{
		".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".gif": "image/gif", ".webp": "image/webp",
	}[ext]
	return data, mime, nil
}

func (a *Agent) frozenImageBytes(attachment messages.ImageAttachment) ([]byte, error) {
	if attachment.BlobHash == "" {
		return append([]byte(nil), attachment.Data...), nil
	}
	p := a.persistenceHandle()
	if p == nil {
		return nil, errors.New("agent: image artifact store is unavailable")
	}
	artifacts, err := p.requestArtifacts()
	if err != nil {
		return nil, err
	}
	return artifacts.Read(session.BlobRef{Hash: attachment.BlobHash, Size: attachment.BlobSize, MediaType: attachment.MediaType}, maxImageBytes)
}

func imageMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// imageContentPartsForMessage uses frozen attachment bytes for canonical
// messages. Legacy messages without ImagesFrozen retain the compatibility
// path-based expansion behavior.
func (a *Agent) imageContentPartsForMessage(workspaceDir string, message messages.Message, budget int64) (parts []llm.ContentPart, used int64, hasImage bool, firstErr error) {
	if !message.ImagesFrozen {
		return imageContentPartsAtBudget(workspaceDir, message.Content, budget)
	}
	matches := markdownImageRe.FindAllStringSubmatchIndex(message.Content, -1)
	if len(matches) == 0 {
		return []llm.ContentPart{{Type: "text", Text: message.Content}}, 0, false, nil
	}
	last := 0
	attachmentIndex := 0
	for _, match := range matches {
		if match[0] > last {
			parts = append(parts, llm.ContentPart{Type: "text", Text: message.Content[last:match[0]]})
		}
		marker := message.Content[match[0]:match[1]]
		path := strings.TrimSpace(message.Content[match[2]:match[3]])
		found := -1
		for i := attachmentIndex; i < len(message.ImageAttachments); i++ {
			if message.ImageAttachments[i].Path == path || message.ImageAttachments[i].Path == "" {
				found = i
				break
			}
		}
		if found < 0 {
			parts = append(parts, llm.ContentPart{Type: "text", Text: marker})
			if firstErr == nil {
				firstErr = fmt.Errorf("frozen image %q has no captured attachment", path)
			}
			last = match[1]
			continue
		}
		attachmentIndex = found + 1
		attachment := message.ImageAttachments[found]
		data, err := a.frozenImageBytes(attachment)
		if err == nil && (budget-used) > 0 && int64(len(data)) <= budget-used {
			mediaType := attachment.MediaType
			if mediaType == "" {
				mediaType = imageMediaType(path)
			}
			parts = append(parts, llm.ContentPart{Type: "image_url", ImageURL: &struct {
				URL string `json:"url"`
			}{URL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)}})
			used += int64(len(data))
			hasImage = true
		} else {
			parts = append(parts, llm.ContentPart{Type: "text", Text: marker})
			if err == nil {
				err = fmt.Errorf("image %q exceeds total attachment limit", path)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		last = match[1]
	}
	if last < len(message.Content) {
		parts = append(parts, llm.ContentPart{Type: "text", Text: message.Content[last:]})
	}
	return parts, used, hasImage, firstErr
}

// planReadOnlyTools is the tool set the model may call while in plan mode.
// Everything that can mutate state (files, shell side effects, git history,
// network) is excluded until the plan is approved.
var planReadOnlyTools = map[string]bool{
	"Read": true, "Glob": true, "Grep": true, "LS": true,
	"TodoWrite": true, "WebSearch": true, "WebFetch": true,
	"GitStatus": true, "GitDiff": true, "GitLog": true,
	"Task": true, "ToolSearch": true, "ReadSkill": true,
	"EnterPlanMode": true, "ExitPlanMode": true, "AskUserQuestion": true,
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
- Use AskUserQuestion for structured clarification when requirements are ambiguous.
- End your reply with the plan only. The user will approve it before execution.`

// buildRequestSnapshotWithPrompt builds the request from one immutable step
// snapshot. Instructions, skills, and tool definitions are bounded values
// captured by beginStepChecked before provider preparation; this function
// never falls back to reading mutable files or extension state for them.
func (a *Agent) buildRequestSnapshotWithPrompt(cfg config.Config, model string, history []messages.Message, sessionID string, sessionStartAt time.Time, plan bool, modelSwitch string, interrupted bool, instructions, skillsSection string, frozenTools []llm.ToolDef) llm.CompletionRequest {
	if sessionID == "" {
		sessionID = a.SessionID()
	}
	if sessionStartAt.IsZero() {
		sessionStartAt = time.Now()
	}
	sys := cfg.SystemPrompt
	sys += fmt.Sprintf("\n\n# Environment\n- Date: %s\n- Timezone: %s\n- OS: %s\n- Shell: %s",
		sessionStartAt.Format("2006-01-02 15:04:05"), time.Local.String(), runtime.GOOS, shellName())
	sys += fmt.Sprintf("\n- Workspace: %s\n- Permission mode: %s\n- Session: %s",
		cfg.Workspace, cfg.PermissionMode, sessionID)
	if modelSwitch != "" {
		sys += "\n\n" + modelSwitch
	}
	if interrupted {
		sys += "\n\n# Interrupted turn\nYour previous turn was interrupted by the user. Tools that were\nrunning may have partially executed and their effects may be incomplete.\nBefore continuing, verify the relevant state (re-read files, re-run git\nstatus or tests) rather than assuming the last known state."
	}
	if plan {
		sys += "\n\n" + PlanModeInstructions
	}
	if instructions != "" {
		sys += "\n\n# Project instructions\n\n" + instructions
	}
	if cfg.MemoryEnabled() {
		if sec := a.MemorySection(); sec != "" {
			sys += sec
		}
	}
	if info, err := workspace.CachedScan(cfg.Workspace, false); err == nil && info != nil {
		a.mu.Lock()
		a.wsInfo = info
		a.mu.Unlock()
		sys += "\n\n# Repository layout\n\n" + info.RepoMap(120)
	}
	if skillsSection != "" {
		sys += skillsSection
	}
	if sec := a.todoSection(); sec != "" {
		sys += "\n\n" + sec
	}
	sys += "\n\n" + availableToolsFromDefs(frozenTools)
	return a.buildRequestFromConfig(cfg, model, sys, history, plan, frozenTools)
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
	a.mu.Lock()
	resources := a.resources
	a.mu.Unlock()
	if resources == nil || resources.Todos == nil {
		return ""
	}
	return resources.Todos.Section()
}

func cloneToolDefs(src []llm.ToolDef) []llm.ToolDef {
	dst := make([]llm.ToolDef, len(src))
	for i, def := range src {
		dst[i] = def
		dst[i].Function.Parameters = cloneMap(def.Function.Parameters)
	}
	return dst
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
		if t.PlanAllowed {
			out = append(out, t)
		}
	}
	return out
}

func (a *Agent) buildRequestFromConfig(cfg config.Config, model, sys string, history []messages.Message, plan bool, frozenTools []llm.ToolDef) llm.CompletionRequest {
	// Hard guarantee: never put an unpaired tool_call / tool message on the
	// wire, whatever a history rewrite left behind (providers 400).
	history = sanitizeToolPairs(history)
	msgs := make([]llm.ChatMessage, 0, len(history)+1)
	msgs = append(msgs, llm.ChatMessage{Role: "system", Content: sys})
	var imageBytes int64

	for _, m := range history {
		switch m.Role {
		case messages.RoleSystem:
			msgs = append(msgs, llm.ChatMessage{Role: "system", Content: m.Content})
		case messages.RoleUser:
			// Multi-modal: canonical inputs use their frozen attachment blobs;
			// legacy messages retain path-based compatibility expansion.
			parts, used, hasImage, _ := a.imageContentPartsForMessage(cfg.Workspace, m, maxAttachmentBytes-imageBytes)
			if hasImage && len(parts) > 0 {
				imageBytes += used
				msgs = append(msgs, llm.ChatMessage{Role: "user", Content: parts})
				continue
			}
			msgs = append(msgs, llm.ChatMessage{Role: "user", Content: m.Content})
		case messages.RoleAssistant:
			cm := llm.ChatMessage{Role: "assistant", Content: m.Content}
			if isDeepSeekModel(cfg, model) {
				cm.ReasoningContent = m.ReasoningContent
			}
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

	// Tool schemas are part of the same frozen step snapshot as the provider
	// binding and history. Never re-read the registry after admission.
	toolDefs := cloneToolDefs(frozenTools)

	req := llm.CompletionRequest{
		// The active model follows fallback switches and /model: the model
		// name must match the endpoint the request is sent to.
		Model:           model,
		ReasoningEffort: cfg.ReasoningEffort,
		Verbosity:       cfg.Verbosity,
		Messages:        msgs,
		Tools:           toolDefs,
		Stream:          true,
	}
	applyGenerationRequest(cfg, &req)
	if cfg.MaxReplyTokens > 0 {
		req.MaxTokens = intPtr(cfg.MaxReplyTokens)
	}
	if plan {
		req.Tools = filterReadOnlyTools(req.Tools)
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

// sanitizeToolPairs repairs tool_call↔tool-result pairing after any history
// rewrite (compact, /remove, /rewind, fork, resume of an older snapshot).
// OpenAI-compatible endpoints reject requests where an
// assistant tool_calls message is not followed by a tool result for every
// call id, or where a tool message has no preceding call — a single dangling
// pair fails EVERY subsequent request and deadlocks the session.
//
// Rules:
//   - an assistant message keeps only the calls that have results immediately
//     following it; calls without results are answered with a synthetic tool
//     result so the protocol stays intact (matching dispatchTools' interrupt
//     behavior);
//   - orphan tool results whose caller is gone are dropped;
//   - prose-only assistant messages survive untouched.
func sanitizeToolPairs(hist []messages.Message) []messages.Message {
	out := make([]messages.Message, 0, len(hist))
	for i := 0; i < len(hist); {
		m := hist[i]
		if m.Role != messages.RoleAssistant || len(m.ToolCalls) == 0 {
			if m.Role == messages.RoleTool {
				// Orphan result: its caller was dropped earlier.
				i++
				continue
			}
			out = append(out, m)
			i++
			continue
		}
		// Collect the consecutive tool results following this assistant.
		callIDs := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			callIDs[tc.ID] = true
		}
		j := i + 1
		answered := make(map[string]bool, len(m.ToolCalls))
		for j < len(hist) && hist[j].Role == messages.RoleTool && callIDs[hist[j].ToolCallID] {
			answered[hist[j].ToolCallID] = true
			j++
		}
		out = append(out, m)
		out = append(out, hist[i+1:j]...)
		// Synthetic results for calls that never got one (their result was
		// dropped, or the assistant message is the last thing in history).
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				out = append(out, messages.NewToolResult(tc,
					"tool result unavailable after a history rewrite; execution state is unknown. Do not re-issue automatically.", true))
			}
		}
		i = j
	}
	return out
}

// truncateResult caps a tool result at maxChars, appending a marker when
// content was cut so the model knows the result was truncated.
func truncateResult(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if len(s) <= maxChars {
		return s
	}
	marker := fmt.Sprintf("\n…[output truncated at %d chars, %d more available]", maxChars, len(s)-maxChars)
	if len(marker) >= maxChars {
		return truncateUTF8Bytes(s, maxChars)
	}
	cut := truncateUTF8Bytes(s, maxChars-len(marker))
	return cut + marker
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
