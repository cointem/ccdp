package tui

import (
	"fmt"

	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

// commandForPolicy returns a copy of the confirmed sandbox/permission policy
// so a pending mutation cannot modify a snapshot retained by the Model.
func (m Model) sandboxPolicy() protocol.SandboxPolicy {
	if m.hasSnapshot {
		p := m.snapshot.Settings.Sandbox
		p.AdditionalDirectories = append([]string(nil), p.AdditionalDirectories...)
		p.AdditionalReadOnlyDirectories = append([]string(nil), p.AdditionalReadOnlyDirectories...)
		p.DisallowedDirectories = append([]string(nil), p.DisallowedDirectories...)
		return p
	}
	return protocol.SandboxPolicy{}
}

func (m Model) permissionPolicy() protocol.PermissionPolicy {
	if m.hasSnapshot {
		p := m.snapshot.Settings.Permission
		p.AlwaysAllow = append([]string(nil), p.AlwaysAllow...)
		p.AlwaysDeny = append([]string(nil), p.AlwaysDeny...)
		return p
	}
	return protocol.PermissionPolicy{Mode: string(m.mode)}
}

// cyclePermissionMode advances the permission mode through the approval-strictness
// ring on shift+tab, mirroring the sibling CLIs. Plan is deliberately excluded: it
// is an execution-mode dimension toggled by /plan, not a permission level, so a
// mode outside the ring restarts at default. The change applies immediately; the
// local mode updates optimistically and the next snapshot confirms it.
func (m *Model) cyclePermissionMode() tea.Cmd {
	// m.mode is refreshed from every authoritative snapshot, so cycling off it
	// keeps rapid shift+tab presses chained correctly even before the round-trip
	// for the previous change lands. Only fall back to the snapshot when no mode
	// has been resolved locally yet.
	current := m.mode
	if current == "" && m.hasSnapshot {
		current = permissions.Mode(m.snapshot.Settings.Permission.Mode)
	}
	ring := []permissions.Mode{permissions.ModeDefault, permissions.ModeAcceptEdits, permissions.ModeBypass}
	next := ring[0]
	for i, mode := range ring {
		if mode == current {
			next = ring[(i+1)%len(ring)]
			break
		}
	}
	policy := m.permissionPolicy()
	policy.Mode = string(next)
	m.mode = next
	return m.submitCommand(protocol.Command{Type: protocol.CommandSetPermissionPolicy,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}, "permission mode → "+next.Label())
}

// modeActive reports whether a non-default permission mode is confirmed, so the
// footer can advertise the shift+tab cycle hint only when it is relevant.
func (m *Model) modeActive() bool {
	mode := m.mode
	if m.hasSnapshot && m.snapshot.Settings.Permission.Mode != "" {
		mode = permissions.Mode(m.snapshot.Settings.Permission.Mode)
	}
	return mode != "" && mode != permissions.ModeDefault
}

func (m Model) availableModels() []string {
	if m.hasSnapshot {
		seen := make(map[string]bool)
		var models []string
		for _, provider := range m.snapshot.Catalog.Providers {
			for _, model := range provider.Models {
				if model != "" && !seen[model] {
					seen[model] = true
					models = append(models, model)
				}
			}
		}
		if len(models) > 0 {
			sort.Strings(models)
			return models
		}
	}
	if m.modelName != "" {
		return []string{m.modelName}
	}
	return nil
}

func (m *Model) executeSelectorAction(action selectorAction, optionID string) tea.Cmd {
	switch action.Kind {
	case selectorEffort, selectorVerbosity:
		value := optionID
		if value == "default" {
			value = ""
		}
		generation := &protocol.SetGeneration{}
		if action.Kind == selectorEffort {
			generation.ReasoningEffort = &value
		} else {
			generation.Verbosity = &value
		}
		return m.submitCommand(protocol.Command{Type: protocol.CommandSetGeneration, Generation: generation}, string(action.Kind)+" → "+optionID)
	case selectorAgent:
		return m.openAgentView(optionID)
	case selectorAgentOutput:
		return m.agentOutputSelection(optionID)
	case selectorModel:
		return m.submitCommand(protocol.Command{Type: protocol.CommandSetModel,
			Model: &protocol.SetModel{Model: optionID}}, "model → "+optionID)
	case selectorMode:
		policy := m.permissionPolicy()
		policy.Mode = optionID
		return m.submitCommand(protocol.Command{Type: protocol.CommandSetPermissionPolicy,
			PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}, "permission mode → "+permissions.Mode(optionID).Label())
	case selectorPlan:
		mode := protocol.ExecutionModeExecute
		if optionID == "on" || optionID == string(protocol.ExecutionModePlan) {
			mode = protocol.ExecutionModePlan
		}
		return m.submitCommand(protocol.Command{Type: protocol.CommandSetExecutionMode,
			ExecutionMode: &protocol.SetExecutionMode{Mode: mode}}, "plan mode → "+optionID)
	case selectorRewind:
		keep, ok := parseRewindOption(optionID)
		if !ok {
			return nil
		}
		return m.submitCommand(protocol.Command{Type: protocol.CommandRewindConversation,
			Rewind: &protocol.RewindConversation{Count: keep}}, fmt.Sprintf("rewind to message %d submitted", keep))
	case selectorResume:
		return m.openResumeSession(optionID)
	}
	return nil
}

func (m *Model) pendingChildInteraction() *protocol.ChildSession {
	if m == nil || m.routing == nil {
		return nil
	}
	for i := range m.routing.rows {
		if childNeedsAttention(m.routing.rows[i]) {
			return &m.routing.rows[i]
		}
	}
	return nil
}

func parseRewindOption(id string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "keep:"))
	return n, err == nil && strings.HasPrefix(id, "keep:") && n >= 0
}
