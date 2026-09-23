package tui

import (
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
	"fmt"
	"github.com/charmbracelet/lipgloss"
	"strconv"
	"strings"
)

func (m *Model) renderHeader() string {
	modelName := m.modelName
	mode := m.mode
	planMode := m.planMode
	sessionID := m.sessionID
	usage := m.usage
	contextWindow := 0
	contextUsed := usage.InputTokens + usage.OutputTokens
	if m.hasSnapshot {
		modelName = m.snapshot.Settings.Model.Model
		if m.snapshot.Settings.Permission.Mode != "" {
			mode = permissions.Mode(m.snapshot.Settings.Permission.Mode)
		}
		planMode = m.snapshot.Settings.ExecutionMode == protocol.ExecutionModePlan
		sessionID = m.snapshot.SessionID.String()
		usage = m.snapshot.Usage
		contextWindow = m.snapshot.Settings.ContextWindow
		contextUsed = m.snapshot.ContextUsedTokens
	}
	var modeColor lipgloss.TerminalColor = colorMuted
	switch mode {
	case "acceptEdits":
		modeColor = colorGo
	case "bypassPermissions":
		modeColor = colorError
	}
	modeBadge := lipgloss.NewStyle().Foreground(modeColor).Bold(true).Render(mode.Label())

	// Plan mode gets its own amber badge.
	if planMode {
		planBadge := styleRunning.Render("plan")
		modeBadge = modeBadge + " " + planBadge
	}

	ws := truncateDisplay(sanitizeANSI(m.workspace), 24)

	hasCost := usage.TurnCount > 0 || usage.InputTokens > 0 || usage.OutputTokens > 0
	parts := make([]string, 0, len(m.statusItems))
	items := m.statusItems
	if sameStatusItems(items, defaultStatusItems) {
		// The persistent footer owns model/effort/context/cost. Keep the header
		// empty after the welcome item, so a normal conversation does not repeat
		// launch identity or settings above and below the transcript.
		items = nil
	}
	for _, item := range items {
		switch item {
		case "version":
			parts = append(parts, styleBrand.Render("ccdp")+" v"+appVersion)
		case "model":
			parts = append(parts, styleAssistant.Render(truncateDisplay(sanitizeANSI(modelName), max(1, m.width))))
		case "mode":
			parts = append(parts, "mode:"+modeBadge)
		case "session":
			parts = append(parts, "session:"+shortID(sanitizeANSI(sessionID)))
		case "workspace":
			parts = append(parts, ws)
		case "cost":
			if hasCost {
				parts = append(parts, fmt.Sprintf("%.1fk→%.1fk · $%.3f",
					float64(usage.InputTokens)/1000,
					float64(usage.OutputTokens)/1000,
					usage.Cost))
			}
		case "context":
			// Keep the runtime's authoritative context estimate visible. The
			// usage counters are request accounting, not the live context
			// projection, so a snapshot always wins above.
			window := contextWindow
			if window <= 0 {
				break
			}
			used := contextUsed
			if used < 0 {
				used = 0
			}
			style := styleContextOK
			if float64(used) >= float64(window)*0.8 {
				style = styleContextWarn
			}
			parts = append(parts, style.Render(formatContextTokens(used)+"/"+formatContextTokens(window)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return fitLines(styleHeader.Render(strings.Join(parts, "  ·  ")), m.width)
}

func sameStatusItems(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// formatContextTokens keeps the context indicator compact without hiding
// zero, sub-percent, or over-capacity estimates. It is intentionally only a
// kilo suffix: the full token count remains legible for ordinary model
// windows and no precision is lost below 1000 tokens.
func formatContextTokens(tokens int) string {
	if tokens < 0 {
		tokens = 0
	}
	if tokens < 1000 {
		return strconv.Itoa(tokens)
	}
	value := strconv.FormatFloat(float64(tokens)/1000, 'f', 1, 64)
	value = strings.TrimSuffix(value, ".0")
	return value + "k"
}

func (m *Model) renderFooter() string {
	state := m.renderPersistentStatus()
	if !m.composerVisible() {
		// Modal-owned keys are intercepted before the composer. Repeating
		// “enter send” underneath an inline decision would describe an action
		// that cannot happen; the decision/selector surface already renders its
		// focused key hints next to the content.
		return indentBlock(state)
	}
	hints := m.footerHints()
	if state == "" || !m.hasSnapshot {
		if hints == "" {
			return ""
		}
		return " " + styleHints.Render(truncateDisplay(hints, max(1, m.width-2)))
	}
	// Prefer one row: the authoritative state on the left, the contextual hints
	// right-aligned. Collapse only when the state is already a single row and the
	// full hint fits beside it, so neither group is truncated. Narrow terminals
	// keep the stacked layout, which lets a wrapped state stay readable. The state
	// arrives padded to the full width, so measure its trimmed content.
	if trimmed := strings.TrimRight(state, " "); !strings.Contains(trimmed, "\n") {
		budget := max(1, m.width-2)
		stateWidth, hintWidth := lipgloss.Width(trimmed), lipgloss.Width(hints)
		if stateWidth+1+hintWidth <= budget {
			gap := budget - stateWidth - hintWidth
			return " " + trimmed + strings.Repeat(" ", gap) + styleHints.Render(hints)
		}
	}
	hintLine := " " + styleHints.Render(truncateDisplay(hints, max(1, m.width-2)))
	return indentBlock(state) + "\n" + hintLine
}

// footerHints is the contextual, right-hand half of the composer footer. It is
// deliberately terse: secondary bindings (newline, paste) live in the composer
// placeholder and /help so the row stays calm beside the persistent state.
func (m *Model) footerHints() string {
	narrow := m.width < 52
	switch {
	case m.quitArmed:
		if narrow {
			return "⌃C again to exit"
		}
		return "⌃C again to exit · any other key to continue"
	case len(m.cmdSug) > 0:
		if narrow {
			return "↑↓ choose · tab · esc"
		}
		return "↑↓ choose · tab complete · enter run · esc close"
	case m.busy:
		return "enter steer · esc interrupt"
	}
	if narrow {
		return "enter send"
	}
	hints := "enter send"
	if len(m.snapshot.Tasks) > 0 {
		// A working task list exists; surface the checklist toggle so it is
		// discoverable without consulting /help.
		hints += " · ⌃t tasks"
	}
	if m.routing != nil && m.sessionID != m.routing.rootID {
		// A child view with siblings advertises quick-switch so the cycling keys
		// are discoverable without opening /agents.
		if _, total := m.agentOrdinal(); total > 1 {
			hints += " · esc main · ⌃←/→ agents"
		}
	}
	return hints
}

// indentBlock strips each row's trailing full-width padding and prefixes it with
// a single space, so footer rows align with the composer's one-cell left padding
// instead of sitting flush-left or carrying invisible padding to the edge.
func indentBlock(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if trimmed := strings.TrimRight(line, " "); trimmed != "" {
			lines[i] = " " + trimmed
		} else {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// renderPersistentStatus is the compact, authoritative session footer. It is
// deliberately independent of /statusline: users must be able to see the
// active model, confirmed reasoning effort, and context estimate while a
// selector, approval, question, or reader is open.
type footerLabels struct{ model, permission, context, cache string }

func (m *Model) renderPersistentStatus() string {
	if m == nil {
		return ""
	}
	text, _ := m.persistentStatusContent()
	return wrapPersistentStatus(text, max(1, m.width-2))
}

func (m *Model) persistentStatusContent() (string, footerLabels) {
	if m == nil {
		return "", footerLabels{}
	}
	modelName := m.modelName
	effort := ""
	used := 0
	window := 0
	threshold := 0.0
	if m.hasSnapshot {
		if m.snapshot.Settings.Model.Model != "" {
			modelName = m.snapshot.Settings.Model.Model
		}
		used = m.snapshot.ContextUsedTokens
		window = m.snapshot.Settings.ContextWindow
		threshold = m.snapshot.Settings.CompactThreshold
		effort = strings.TrimSpace(m.snapshot.Settings.ReasoningEffort)
	}
	if used == 0 && m.routing != nil && m.sessionID != m.routing.rootID {
		for _, row := range m.routing.rows {
			if string(row.SessionID) == m.sessionID {
				tokens := row.Run.Usage.InputTokens + row.Run.Usage.OutputTokens
				if tokens > 0 {
					used = tokens
				}
				break
			}
		}
	}
	if window == 0 && m.routing != nil {
		if root, ok := m.routing.views[m.routing.rootID]; ok && root.snapshot.Settings.ContextWindow > 0 {
			window = root.snapshot.Settings.ContextWindow
		}
	}
	if modelName == "" {
		modelName = "model"
	}
	if effort == "" {
		effort = "default"
	}
	if used < 0 {
		used = 0
	}
	ratio := "—/—"
	if m.hasSnapshot {
		ratio = formatContextTokens(used) + "/—"
	}
	if window > 0 {
		ratio = formatContextTokens(used) + "/" + formatContextTokens(window)
		if m.width >= 60 {
			pct := int(float64(used) * 100.0 / float64(window))
			ratio = fmt.Sprintf("%d%% (%s)", pct, ratio)
		}
	}
	mode := string(m.mode)
	if mode == "" {
		mode = "acceptEdits"
	}
	if m.hasSnapshot && m.snapshot.Settings.Permission.Mode != "" {
		mode = string(m.snapshot.Settings.Permission.Mode)
	}

	modelName = compactModelName(sanitizeANSI(modelName), max(1, m.width))

	parts := []string{modelName}
	if effort != "default" {
		parts = append(parts, effortLabel(effort))
	}
	modeDisplay := permissions.Mode(mode).Label()
	if m.planMode {
		modeDisplay += " · plan"
	}
	modeStyle := styleStatus
	if modeDisplay == "bypass" {
		modeStyle = styleToolErr
	}
	parts = append(parts, modeStyle.Render(modeDisplay))
	if warn, over := compactWarning(used, window, threshold); warn != "" && m.width >= 60 {
		style := styleContextWarn
		if over {
			style = styleToolErr
		}
		parts = append(parts, style.Render(warn))
	}
	ratioStyle := styleContextOK
	if window > 0 && float64(used) >= float64(window)*0.8 {
		ratioStyle = styleContextWarn
	}
	parts = append(parts, ratioStyle.Render(ratio))
	parts = append(parts, styleStatus.Render(cacheLabel(m.sessionCache())))
	line := strings.Join(parts[:len(parts)-1], styleDivider.Render(" · ")) + styleDivider.Render(" · ") + parts[len(parts)-1]
	return line, footerLabels{model: modelName, permission: permissions.Mode(mode).Label(), context: ratio, cache: cacheLabel(m.sessionCache())}
}

// compactWarning is a proactive "auto-compaction approaching" hint built only
// from real data: the configured context window and the runtime's compaction
// threshold (the same ratio the agent uses to decide when to compact). It
// returns a human label and whether the threshold is already crossed. When the
// window, usage or threshold is unknown it stays silent — it never estimates a
// percentage the runtime does not actually enforce.
func compactWarning(used, window int, threshold float64) (string, bool) {
	if window <= 0 || threshold <= 0 || threshold > 1 {
		return "", false
	}
	limit := float64(window) * threshold
	// Warn only within the last 15% of headroom before the agent compacts.
	if float64(used) < limit*0.85 {
		return "", false
	}
	pctLeft := int((limit - float64(used)) * 100.0 / float64(window))
	if pctLeft <= 0 {
		return "⚠ auto-compact now", true
	}
	return fmt.Sprintf("⚠ %d%% to auto-compact", pctLeft), false
}

// effortLabel pairs the reasoning-effort word with a density glyph (○◐●◉) so
// the level reads at a glance, matching the sibling CLIs. Unknown or default
// effort renders as the bare word, which keeps the confirmed effort text intact.
func effortLabel(effort string) string {
	if glyph := effortGlyph(effort); glyph != "" {
		return glyph + " " + effort
	}
	return effort
}

func effortGlyph(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "minimal":
		return "○"
	case "medium":
		return "◐"
	case "high":
		return "●"
	case "max", "maximum":
		return "◉"
	default:
		return ""
	}
}

func compactModelName(name string, width int) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "model"
	}
	// Provider prefixes are useful in details but consume the exact cells that
	// make the footer readable on a narrow terminal. Keep the final component
	// when a slash-qualified name cannot fit as a whole.
	if lipgloss.Width(name) > width/2 && strings.Contains(name, "/") {
		parts := strings.Split(name, "/")
		name = parts[len(parts)-1]
	}
	return truncateDisplay(name, max(1, width/2))
}

func (m *Model) sessionCache() protocol.CacheStats {
	usage := m.usage
	if m.hasSnapshot {
		usage = m.snapshot.Usage
	}
	cache := usage.Cache
	if usage.TurnCount == 0 && usage.InputTokens == 0 {
		cache.Tracking = true
	}
	return cache
}

func cacheLabel(c protocol.CacheStats) string {
	if !c.Tracking {
		return "cache ?"
	}
	if c.UnknownHistory || c.ReportedInputTokens != c.InputTokens || c.CachedTokens < 0 || c.CachedTokens > c.InputTokens {
		return "cache ?"
	}
	if c.InputTokens == 0 {
		return "cache —"
	}
	return fmt.Sprintf("cache %.0f%%", float64(c.CachedTokens)*100/float64(c.InputTokens))
}

func cacheDetail(c protocol.CacheStats) string {
	label := cacheLabel(c)
	if label == "cache ?" {
		return "Session cache: unknown (missing or incomplete provider data)"
	}
	if c.InputTokens == 0 {
		return "Session cache: no input usage yet"
	}
	return fmt.Sprintf("Session cache: %s / %s input tokens (%.1f%%)", formatNumber(c.CachedTokens), formatNumber(c.InputTokens), float64(c.CachedTokens)*100/float64(c.InputTokens))
}
