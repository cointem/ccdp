package tui

import (
	"fmt"
	"strings"

	"ccdp/internal/protocol"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type questionState struct {
	request               *protocol.QuestionRequest
	answers               []protocol.QuestionAnswer
	index, cursor, scroll int
	editing               bool
	input                 textinput.Model
	errorText             string
	pending               protocol.CommandID
}

func (m *Model) setQuestion(request *protocol.QuestionRequest) {
	if request == nil {
		m.question = nil
		return
	}
	if m.question != nil && m.question.request.ID == request.ID {
		return
	}
	if protocol.ValidateQuestions(request.Questions) != nil {
		return
	}
	input := textinput.New()
	input.Prompt = "Other / detail: "
	input.CharLimit = 2000
	m.question = &questionState{request: protocol.CloneQuestionRequest(request), answers: make([]protocol.QuestionAnswer, len(request.Questions)), input: input}
}

func (m *Model) handleQuestionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	state := m.question
	if state == nil {
		return m, nil
	}
	if state.pending != "" {
		return m, nil
	}
	q := state.request.Questions[state.index]
	state.errorText = ""
	switch msg.String() {
	case "esc":
		return m, m.submitQuestion(true)
	case "ctrl+c":
		return m, m.submitCommand(protocol.Command{Type: protocol.CommandInterrupt}, "interrupt requested")
	case "tab":
		state.editing = !state.editing
		if state.editing {
			return m, state.input.Focus()
		}
		state.input.Blur()
		return m, nil
	case "ctrl+p":
		if state.index > 0 {
			state.answers[state.index].Text = state.input.Value()
			state.index--
			state.restoreCursor()
			state.scroll = 0
			state.input.SetValue(state.answers[state.index].Text)
			if state.answers[state.index].Text != "" && len(state.answers[state.index].Selected) == 0 {
				state.editing = true
				state.input.Focus()
			} else {
				state.editing = false
				state.input.Blur()
			}
		}
		return m, nil
	case "pgup":
		state.scroll = max(0, state.scroll-3)
		return m, nil
	case "pgdown":
		state.scroll += 3
		return m, nil
	case "up", "down":
		if !state.editing {
			if msg.String() == "up" {
				state.cursor = (state.cursor + len(q.Options) - 1) % len(q.Options)
			} else {
				state.cursor = (state.cursor + 1) % len(q.Options)
			}
			state.scroll = -1
			return m, nil
		}
	case " ":
		if !state.editing {
			state.toggle()
			return m, nil
		}
	case "enter":
		state.answers[state.index].Text = state.input.Value()
		if !state.editing && !q.MultiSelect {
			state.answers[state.index].Selected = []string{q.Options[state.cursor].Label}
		}
		answer := state.answers[state.index]
		if len(answer.Selected) == 0 && strings.TrimSpace(answer.Text) == "" {
			state.errorText = "Select an option (Space) or enter text (Tab)."
			return m, nil
		}
		if state.index+1 < len(state.answers) {
			state.index++
			state.cursor, state.scroll = 0, 0
			state.input.SetValue(state.answers[state.index].Text)
			state.editing = false
			state.input.Blur()
			return m, nil
		}
		return m, m.submitQuestion(false)
	}
	if state.editing {
		var cmd tea.Cmd
		state.input, cmd = state.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (s *questionState) toggle() {
	q := s.request.Questions[s.index]
	label := q.Options[s.cursor].Label
	answer := &s.answers[s.index]
	for i, value := range answer.Selected {
		if value == label {
			answer.Selected = append(answer.Selected[:i], answer.Selected[i+1:]...)
			return
		}
	}
	if !q.MultiSelect {
		answer.Selected = nil
	}
	answer.Selected = append(answer.Selected, label)
}

func (m *Model) submitQuestion(cancelled bool) tea.Cmd {
	s := m.question
	answer := protocol.AnswerQuestion{RequestID: s.request.ID, Cancelled: cancelled}
	if !cancelled {
		answer.Answers = s.answers
	}
	if err := s.request.ValidateAnswer(answer); err != nil {
		s.errorText = err.Error()
		return nil
	}
	cmd := protocol.Command{ID: nextUICommandID(), SessionID: protocol.SessionID(m.sessionID), Type: protocol.CommandAnswerQuestion, Answer: &answer}
	s.pending = cmd.ID
	return m.submitCommand(cmd, "answer submitted")
}

func (m *Model) renderQuestion() string {
	s := m.question
	if s == nil {
		return ""
	}
	q := s.request.Questions[s.index]
	width := min(76, max(10, m.width-6))
	inner := max(6, width-4)
	lines := wrapDisplay(q.Question, inner)
	selectedRow := 0
	for i, option := range q.Options {
		if !s.editing && s.cursor == i {
			selectedRow = len(lines)
		}
		cursor, mark := " ", "( )"
		if q.MultiSelect {
			mark = "[ ]"
		}
		if !s.editing && s.cursor == i {
			cursor = ">"
		}
		for _, label := range s.answers[s.index].Selected {
			if label == option.Label {
				if q.MultiSelect {
					mark = "[x]"
				} else {
					mark = "(x)"
				}
			}
		}
		lines = append(lines, wrapDisplay(fmt.Sprintf("%s %s %s — %s", cursor, mark, option.Label, option.Description), inner)...)
	}
	hint := "↑↓ choose · Space toggle · Tab text · Enter next/submit · Ctrl+P back · Esc cancel"
	hint = "PgUp/PgDn scroll · " + hint
	rows := max(1, m.height-7-len(wrapDisplay(hint, inner)))
	if s.scroll < 0 {
		s.scroll = max(0, selectedRow-rows+1)
	}
	offset := min(s.scroll, max(0, len(lines)-rows))
	s.scroll = offset
	var b strings.Builder
	b.WriteString(truncateDisplay(fmt.Sprintf("%s · %d/%d", q.Header, s.index+1, len(s.answers)), inner) + "\n")
	b.WriteString(strings.Join(lines[offset:min(len(lines), offset+rows)], "\n") + "\n")
	s.input.Width = max(1, inner-16)
	b.WriteString(truncateDisplay(s.input.View(), inner) + "\n")

	if s.pending != "" {
		hint = "Submitting answer…"
	}
	for _, line := range wrapDisplay(hint, inner) {
		b.WriteString(line + "\n")
	}
	if s.errorText != "" {
		b.WriteString(truncateDisplay(s.errorText, inner))
	}
	return styleModal.Width(width).Render(strings.TrimSpace(b.String()))
}

func (s *questionState) restoreCursor() {
	if s == nil || s.request == nil || s.index < 0 || s.index >= len(s.request.Questions) {
		return
	}
	q := s.request.Questions[s.index]
	for _, selected := range s.answers[s.index].Selected {
		for i, option := range q.Options {
			if option.Label == selected {
				s.cursor = i
				return
			}
		}
	}
	s.cursor = 0
}
