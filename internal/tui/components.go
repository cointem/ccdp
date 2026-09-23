package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// ScrollState is the small, shared scroll model used by transcript, selectors,
// and review/approval panels.  Keeping the bounds in one place is important
// on a resized or very narrow terminal: callers can move the cursor first and
// then render without having to duplicate clamping rules.
type ScrollState struct {
	Offset   int
	Viewport int
	Content  int
}

func (s *ScrollState) Set(viewport, content int) {
	if s == nil {
		return
	}
	s.Viewport = max(1, viewport)
	s.Content = max(0, content)
	s.Offset = min(max(0, s.Offset), s.MaxOffset())
}

func (s ScrollState) MaxOffset() int {
	return max(0, s.Content-s.Viewport)
}

func (s *ScrollState) Move(delta int) {
	if s == nil {
		return
	}
	s.Offset = min(max(0, s.Offset+delta), s.MaxOffset())
}

func (s *ScrollState) PageUp() {
	if s == nil {
		return
	}
	s.Move(-max(1, s.Viewport))
}

func (s *ScrollState) PageDown() {
	if s == nil {
		return
	}
	s.Move(max(1, s.Viewport))
}

func (s *ScrollState) Home() {
	if s != nil {
		s.Offset = 0
	}
}

func (s *ScrollState) End() {
	if s != nil {
		s.Offset = s.MaxOffset()
	}
}

func (s *ScrollState) EnsureVisible(index int) {
	if s == nil {
		return
	}
	if index < s.Offset {
		s.Offset = index
	}
	if index >= s.Offset+s.Viewport {
		s.Offset = index - s.Viewport + 1
	}
	s.Offset = min(max(0, s.Offset), s.MaxOffset())
}

// Focus identifies the one component allowed to consume a key.  Modal
// components are checked before the composer; this prevents an approval or a
// selector key from leaking into the input buffer.
type Focus int

const (
	FocusComposer Focus = iota
	FocusTranscript
	FocusSelector
	FocusApproval
	FocusQuestion
	FocusHistorySearch
	FocusReader
	FocusExit
)

// PanelManager is deliberately stateless.  Bubble Tea copies the root model on
// every update, so routing derived from the current model avoids a second
// mutable focus owner that could get out of sync with a closed modal.
type PanelManager struct{}

func (PanelManager) Target(m *Model) Focus {
	if m == nil {
		return FocusComposer
	}
	switch m.activePanel() {
	case panelExit:
		return FocusExit
	case panelReader:
		return FocusReader
	case panelHistorySearch:
		return FocusHistorySearch
	case panelSelector:
		return FocusSelector
	case panelQuestion:
		return FocusQuestion
	case panelApproval:
		return FocusApproval
	}
	if !m.followOutput {
		return FocusTranscript
	}
	return FocusComposer
}

// Route handles only modal-owned keys.  Returning handled=false leaves
// transcript/composer navigation to the root key handler, preserving the
// existing textarea and history behavior.
func (r PanelManager) Route(m *Model, msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m == nil {
		return m, nil, false
	}
	switch r.Target(m) {
	case FocusExit:
		switch msg.String() {
		case "y":
			return m, tea.Quit, true
		case "esc", "n", "ctrl+c":
			m.exitConfirm = false
		}
		return m, nil, true
	case FocusReader:
		model, cmd := m.handleReaderKey(msg)
		return model, cmd, true
	case FocusHistorySearch:
		model, cmd, _ := m.handleHistorySearchKey(msg)
		return model, cmd, true

	case FocusQuestion:
		model, cmd := m.handleQuestionKey(msg)
		return model, cmd, true
	case FocusApproval:
		model, cmd := m.handleApprovalKey(msg)
		return model, cmd, true
	case FocusSelector:
		model, cmd := m.handlePickerKey(msg)
		return model, cmd, true
	default:
		return m, nil, false
	}
}

// SelectorOption is one stable, typed option in a selection panel.
type SelectorOption struct {
	ID          string
	Label       string
	Description string
	Current     bool
	Disabled    bool
}

type Selector struct {
	Title   string
	Options []SelectorOption
	Index   int
	Scroll  ScrollState
}

func (s *Selector) SetSize(viewport int) {
	if s == nil {
		return
	}
	s.Scroll.Set(viewport, len(s.Options))
	s.Scroll.EnsureVisible(s.Index)
}

func (s *Selector) Move(delta int) {
	if s == nil || len(s.Options) == 0 {
		return
	}
	s.Index = min(max(0, s.Index+delta), len(s.Options)-1)
	s.Scroll.EnsureVisible(s.Index)
}

func (s Selector) Selected() (SelectorOption, bool) {
	if s.Index < 0 || s.Index >= len(s.Options) {
		return SelectorOption{}, false
	}
	return s.Options[s.Index], true
}
