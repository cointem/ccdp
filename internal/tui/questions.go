package tui

import (
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
	deferred              bool
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
	if state.pending != "" && msg.String() != "esc" && msg.String() != "ctrl+c" {
		return m, nil
	}
	q := state.request.Questions[state.index]
	state.errorText = ""
	switch msg.String() {
	case "esc":
		state.deferred = true
		return m, nil
	case "ctrl+c":
		return m, m.requestInterrupt()
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
	if m.watchDisconnected {
		s.errorText = "Disconnected: answer preserved. Use Esc then /reconnect."
		return nil
	}
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
