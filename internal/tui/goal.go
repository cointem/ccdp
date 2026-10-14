package tui

import (
	"fmt"
	"strings"

	"ccdp/internal/commands"
	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) runGoalCommand(raw string) (tea.Model, tea.Cmd) {
	if m.routing != nil && m.sessionID != m.routing.rootID {
		m.pushStatus("/goal belongs to the main session; use /root first")
		return m, nil
	}
	goal, err := commands.ParseGoal(raw)
	if err != nil {
		m.pushLog("error", err.Error())
		return m, nil
	}
	if goal == nil {
		return m, m.runQuery(protocol.QueryGoal, "goal report requested")
	}
	return m, m.submitCommand(protocol.Command{Type: protocol.CommandGoal, Goal: goal}, "goal "+goal.Action+" submitted")
}

func goalStatus(goal *protocol.Goal) string {
	if goal == nil {
		return ""
	}
	text := fmt.Sprintf("goal %s · %d tokens", goal.Status, goal.TokensUsed)
	if goal.TokenBudget > 0 {
		text = fmt.Sprintf("goal %s · %d/%d tokens", goal.Status, goal.TokensUsed, goal.TokenBudget)
	}
	if goal.TokensEstimated {
		text += " (estimated)"
	}
	if goal.Status != "active" && strings.TrimSpace(goal.Reason) != "" {
		return text + " · " + sanitizeANSI(strings.Join(strings.Fields(goal.Reason), " "))
	}
	return text + " · " + sanitizeANSI(strings.Join(strings.Fields(goal.Objective), " "))
}
