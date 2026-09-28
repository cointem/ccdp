package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const agentShelfGrace = 30 * time.Second

type agentShelfState struct {
	run       protocol.RunID
	deadline  time.Time
	dismissed bool
}

// Shelf visibility is frontend state. Hiding a row never deletes the session,
// cancels work, or acknowledges a result on behalf of the main agent.
func (m *Model) syncAgentShelf() {
	if m.routing == nil {
		return
	}
	if m.routing.agentShelf == nil {
		m.routing.agentShelf = make(map[protocol.SessionID]agentShelfState)
	}
	present := make(map[protocol.SessionID]bool, len(m.routing.rows))
	for _, row := range m.routing.rows {
		present[row.SessionID] = true
		s, exists := m.routing.agentShelf[row.SessionID]
		if !exists || s.run != row.Run.ID {
			s = agentShelfState{run: row.Run.ID}
		}
		if row.Run.Active() {
			s.deadline, s.dismissed = time.Time{}, false
		} else if s.deadline.IsZero() && string(row.SessionID) != m.sessionID && !s.dismissed {
			s.deadline = row.Run.FinishedAt.Add(agentShelfGrace)
			if row.Run.FinishedAt.IsZero() {
				s.deadline = now().Add(agentShelfGrace)
			}
		}
		m.routing.agentShelf[row.SessionID] = s
	}
	for id := range m.routing.agentShelf {
		if !present[id] {
			delete(m.routing.agentShelf, id)
		}
	}
}

func (m *Model) retainAgentShelf(id string) {
	if m.routing == nil {
		return
	}
	m.syncAgentShelf()
	if s, ok := m.routing.agentShelf[protocol.SessionID(id)]; ok {
		s.deadline, s.dismissed = time.Time{}, false
		m.routing.agentShelf[protocol.SessionID(id)] = s
	}
}

func (m *Model) releaseAgentShelf(id string) {
	if m.routing == nil {
		return
	}
	if s, ok := m.routing.agentShelf[protocol.SessionID(id)]; ok && !s.dismissed {
		s.deadline = now().Add(agentShelfGrace)
		m.routing.agentShelf[protocol.SessionID(id)] = s
	}
}

type agentShelfLine struct {
	text, target string
	canDismiss   bool
}

func (m *Model) agentShelfLines() []agentShelfLine {
	if m.routing == nil || m.width < 12 || m.height < 12 {
		return nil
	}
	var rows []protocol.ChildSession
	for _, row := range m.agentSpawnOrder() {
		// Internal guardians remain in the full directory, not the task shelf.
		if row.Purpose == "guardian" {
			continue
		}
		s, ok := m.routing.agentShelf[row.SessionID]
		viewed := string(row.SessionID) == m.sessionID
		visible := row.Run.Active() || viewed || (!s.dismissed && row.Run.SaveError != "")
		if !visible && !s.dismissed {
			deadline := s.deadline
			if !ok {
				deadline = row.Run.FinishedAt.Add(agentShelfGrace)
			}
			visible = !deadline.IsZero() && now().Before(deadline)
		}
		if visible {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	// Keep the viewed row and actionable work reachable even with many agents.
	rank := func(r protocol.ChildSession) int {
		if string(r.SessionID) == m.sessionID {
			return 0
		}
		if childAwaitingApproval(r) {
			return 1
		}
		if r.Run.Active() {
			return 2
		}
		return 3
	}
	sort.SliceStable(rows, func(i, j int) bool { return rank(rows[i]) < rank(rows[j]) })
	limit := min(3, max(1, m.height/6-1))
	main := "○ main · 点击切换 · Ctrl+A 全部"
	if m.sessionID == m.routing.rootID {
		main = "● main · 点击切换 · Ctrl+A 全部"
	}
	lines := []agentShelfLine{{text: main, target: m.routing.rootID}}
	for _, row := range rows[:min(limit, len(rows))] {
		status := map[string]string{"queued": "排队中", "starting": "准备中", "running": "运行中", "waiting_approval": "等待确认", "settling": "正在保存", "partial": "部分完成", "succeeded": "运行结束", "failed": "失败", "cancelled": "已取消", "interrupted": "已中断"}[row.Run.Status]
		if status == "" {
			status = row.Run.Status
		}
		mark := "○"
		if row.Run.Active() || string(row.SessionID) == m.sessionID {
			mark = "●"
		}
		elapsed := ""
		if !row.Run.StartedAt.IsZero() {
			end := row.Run.FinishedAt
			if row.Run.Active() && end.IsZero() {
				end = now()
			}
			if !end.IsZero() {
				elapsed = " · " + formatElapsed(max(time.Duration(0), end.Sub(row.Run.StartedAt)))
			}
		}
		lines = append(lines, agentShelfLine{text: fmt.Sprintf("%s %s%s · %s", mark, status, elapsed, strings.Join(strings.Fields(childDisplayName(row)), " ")), target: string(row.SessionID), canDismiss: !row.Run.Active()})
	}
	if len(rows) > limit {
		lines = append(lines, agentShelfLine{text: fmt.Sprintf("… 另有 %d 个 · 点击查看全部", len(rows)-limit), target: ""})
	}
	return lines
}

func (m *Model) renderAgentShelf() string {
	var rendered []string
	for _, line := range m.agentShelfLines() {
		width := m.width - 2
		if line.canDismiss {
			width -= 4
		}
		text := truncateDisplay(line.text, width)
		if line.canDismiss {
			text += strings.Repeat(" ", max(0, width-lipgloss.Width(text))) + " [×]"
		}
		if line.target == m.sessionID {
			text = lipgloss.NewStyle().Bold(true).Render(text)
		} else {
			text = styleHints.Render(text)
		}
		rendered = append(rendered, " "+text)
	}
	return strings.Join(rendered, "\n")
}

func (m *Model) clickAgentShelf(x, y int) (bool, tea.Cmd) {
	lines := m.agentShelfLines()
	i := y - (m.height - len(lines))
	if i < 0 || i >= len(lines) || x < 1 || x >= m.width {
		return false, nil
	}
	line := lines[i]
	if line.canDismiss && x >= m.width-5 {
		s := m.routing.agentShelf[protocol.SessionID(line.target)]
		s.dismissed = true
		m.routing.agentShelf[protocol.SessionID(line.target)] = s
		if line.target == m.sessionID {
			return true, m.openAgentView(m.routing.rootID)
		}
		m.syncViewportHeight()
		return true, nil
	}
	if line.target == "" {
		return true, m.agentPicker()
	}
	return true, m.openAgentView(line.target)
}
