package tui

import (
	"strings"
	"testing"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func uiQuestionRequest() *protocol.QuestionRequest {
	options := []protocol.QuestionOption{{Label: "A", Description: "First"}, {Label: "B", Description: "Second"}}
	return &protocol.QuestionRequest{ID: "q-1", Questions: []protocol.Question{{Question: "Choose database", Header: "Database", Options: options}, {Question: "Choose features", Header: "Features", Options: options, MultiSelect: true}}}
}

func TestQuestionUIAnswersDoNotUseApproval(t *testing.T) {
	m := sugModel()
	m.width, m.height = 80, 24
	m.setQuestion(uiQuestionRequest())
	m.textarea.SetValue("preserved draft")
	key := func(msg tea.KeyMsg) tea.Cmd { _, cmd := m.handleQuestionKey(msg); return cmd }
	if (FocusRouter{}).Target(m) != FocusQuestion {
		t.Fatal("question did not own input")
	}
	key(tea.KeyMsg{Type: tea.KeyDown})
	key(tea.KeyMsg{Type: tea.KeyEnter})
	if m.question.index != 1 || m.question.answers[0].Selected[0] != "B" {
		t.Fatal("single select lost")
	}
	if cmd := key(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || m.question.errorText == "" {
		t.Fatal("empty multi select submitted")
	}
	key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	key(tea.KeyMsg{Type: tea.KeyDown})
	key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	key(tea.KeyMsg{Type: tea.KeyTab})
	key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("支持中文")})
	cmd := key(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("missing answer command")
	}
	cmd()
	client := m.client.(*recordingClient)
	if len(client.submits) != 1 {
		t.Fatal("missing submission")
	}
	submitted := client.submits[0]
	if submitted.Type != protocol.CommandAnswerQuestion || submitted.Approval != nil {
		t.Fatal("answer routed through approval")
	}
	if len(submitted.Answer.Answers[1].Selected) != 2 || submitted.Answer.Answers[1].Text != "支持中文" {
		t.Fatalf("answer lost: %+v", submitted.Answer)
	}
	if key(tea.KeyMsg{Type: tea.KeyEnter}) != nil {
		t.Fatal("duplicate UI submission")
	}
	if m.textarea.Value() != "preserved draft" {
		t.Fatal("question consumed composer draft")
	}
}

func TestQuestionSnapshotPreservesDraftAndCancellation(t *testing.T) {
	m := sugModel()
	m.setQuestion(uiQuestionRequest())
	m.question.input.SetValue("draft")
	m.setQuestion(uiQuestionRequest())
	if m.question.input.Value() != "draft" {
		t.Fatal("snapshot reset draft")
	}
	_, cmd := m.handleQuestionKey(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("no cancellation command")
	}
	cmd()
	answer := m.client.(*recordingClient).submits[0].Answer
	if !answer.Cancelled || len(answer.Answers) != 0 {
		t.Fatal("cancelled question fabricated answers")
	}
	m.setQuestion(nil)
	if m.question != nil {
		t.Fatal("cancelled snapshot left modal open")
	}
}

func TestQuestionModalFitsTerminal(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		m := sugModel()
		m.width, m.height = width, 24
		request := uiQuestionRequest()
		request.Questions[0].Question = strings.Repeat("较长的问题文字 ", 20)
		m.setQuestion(request)
		rendered := m.renderQuestion()
		if lipgloss.Width(rendered) > width || lipgloss.Height(rendered) > m.height {
			t.Fatalf("modal %dx%d exceeds %dx%d", lipgloss.Width(rendered), lipgloss.Height(rendered), width, m.height)
		}
	}
}

func TestGenerationSlashCommands(t *testing.T) {
	for _, line := range []string{"/effort high", "/verbosity low", "/effort default"} {
		m := sugModel()
		_, cmd := m.runCommand(line)
		if cmd == nil {
			t.Fatalf("no command for %s", line)
		}
		cmd()
		command := m.client.(*recordingClient).submits[0]
		if command.Type != protocol.CommandSetGeneration {
			t.Fatal(command.Type)
		}
		if line == "/effort default" && (command.Generation.ReasoningEffort == nil || *command.Generation.ReasoningEffort != "") {
			t.Fatal("default did not clear override")
		}
	}
}
