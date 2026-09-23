package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// maxFileMentions caps how many candidates are collected for one @-completion.
// The visible window is smaller still (see cmdSuggestionPageSize) and scrolls.
const maxFileMentions = 50

// fileSuggestion is one @-completion candidate. insert replaces the typed query
// (the text after '@'); label is what the popup shows; dir marks a directory so
// the completion ends with '/' and drills in on the next keystroke.
type fileSuggestion struct {
	label  string
	insert string
	dir    bool
}

// fileMention is the transient @-completion state. token is the full trailing
// composer token including '@'; query is that token without the '@'.
type fileMention struct {
	token string
	query string
	items []fileSuggestion
	idx   int
}

// trailingToken returns the run of non-whitespace characters at the end of the
// composer value. @-completion is scoped to this trailing token, which is where
// a mention is naturally typed; it avoids fragile mid-buffer cursor math.
func trailingToken(val string) string {
	idx := strings.LastIndexFunc(val, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n'
	})
	return val[idx+1:]
}

// buildFileMention lists one directory level matching the typed query. A query
// is split at its last '/': the head selects the directory (relative to the
// workspace, or absolute when the query starts with '/') and the tail is a
// case-insensitive name prefix. It returns nil when the token is not a mention
// or the directory cannot be listed, so the popup simply stays closed.
func buildFileMention(workspace, token string) *fileMention {
	if token == "" || token[0] != '@' || strings.ContainsAny(token, " \t\n") {
		return nil
	}
	query := token[1:]
	dirPart, prefix := "", query
	if slash := strings.LastIndex(query, "/"); slash >= 0 {
		dirPart, prefix = query[:slash+1], query[slash+1:]
	}
	var listDir string
	if strings.HasPrefix(query, "/") {
		listDir = filepath.Clean(dirPart)
	} else {
		if strings.TrimSpace(workspace) == "" {
			workspace = "."
		}
		listDir = filepath.Join(workspace, dirPart)
	}
	entries, err := os.ReadDir(listDir)
	if err != nil {
		return nil
	}
	lowerPrefix := strings.ToLower(prefix)
	items := make([]fileSuggestion, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if lowerPrefix != "" && !strings.HasPrefix(strings.ToLower(name), lowerPrefix) {
			continue
		}
		item := fileSuggestion{label: name, insert: dirPart + name, dir: e.IsDir()}
		if e.IsDir() {
			item.label += "/"
			item.insert += "/"
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].dir != items[j].dir {
			return items[i].dir // directories first, so drilling in is quick
		}
		return items[i].label < items[j].label
	})
	if len(items) > maxFileMentions {
		items = items[:maxFileMentions]
	}
	return &fileMention{token: token, query: query, items: items}
}

// refreshFileMention recomputes the @-completion popup from the composer. The
// slash-command popup owns the input when it is open, so the two never overlap.
func (m *Model) refreshFileMention() {
	m.closeFileMention()
	if len(m.cmdSug) > 0 {
		return
	}
	fm := buildFileMention(m.workspace, trailingToken(m.textarea.Value()))
	if fm == nil {
		return
	}
	m.fileMention = fm
	m.syncViewportHeight()
}

func (m *Model) closeFileMention() {
	if m.fileMention != nil {
		m.fileMention = nil
		m.syncViewportHeight()
	}
}

func (m *Model) moveFileMention(delta int) {
	fm := m.fileMention
	if fm == nil || len(fm.items) == 0 {
		return
	}
	fm.idx = (fm.idx + delta + len(fm.items)) % len(fm.items)
}

// acceptFileMention replaces the trailing token with the highlighted candidate.
// Directories reopen the popup so the user can keep drilling; files close it and
// leave the mention ready to send.
func (m *Model) acceptFileMention() {
	fm := m.fileMention
	if fm == nil || fm.idx < 0 || fm.idx >= len(fm.items) {
		m.closeFileMention()
		return
	}
	item := fm.items[fm.idx]
	newVal := strings.TrimSuffix(m.textarea.Value(), fm.token) + "@" + item.insert
	m.textarea.SetValue(newVal)
	m.textarea.CursorEnd()
	m.syncInputHeight()
	m.closeFileMention()
	if item.dir {
		m.refreshFileMention()
	}
}

// renderFileMention draws the @-completion popup above the composer, reusing the
// command-suggestion chrome and colour-only selection so the two popups feel
// identical. A scroll hint appears when candidates exceed the visible window.
func (m *Model) renderFileMention() string {
	fm := m.fileMention
	if fm == nil || len(fm.items) == 0 {
		return ""
	}
	available := 4
	if m.width > 0 {
		available = max(4, m.width-6)
	}
	pageSize := m.cmdSuggestionPageSize()
	start, end := 0, min(len(fm.items), pageSize)
	if fm.idx >= end {
		end = min(len(fm.items), fm.idx+1)
		start = max(0, end-pageSize)
	}
	var sb strings.Builder
	for i, item := range fm.items[start:end] {
		absolute := start + i
		line := truncateDisplay(item.label, max(1, available-2))
		if absolute == fm.idx {
			line = "❯ " + styleCmdSugSel.Render(line)
		} else {
			line = "  " + styleCmdSug.Render(line)
		}
		sb.WriteString(line + "\n")
	}
	if len(fm.items) > end-start {
		sb.WriteString("  " + styleCmdSug.Render("↑/↓ "+
			strconv.Itoa(start+1)+"–"+strconv.Itoa(end)+" of "+strconv.Itoa(len(fm.items))) + "\n")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true, false, false, false).
		BorderForeground(colorBorder).
		Padding(0, 1).
		Render(strings.TrimRight(sb.String(), "\n")) + "\n"
}
