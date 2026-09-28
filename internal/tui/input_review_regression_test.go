package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

func TestReviewUnicodeReverseSearchBackspace(t *testing.T) {
	m := sugModel()
	m.history = []string{"中文"}
	m.startHistorySearch()
	m.handleHistorySearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文")})
	m.handleHistorySearchKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if !utf8.ValidString(m.histSearch.query) || m.histSearch.query != "中" {
		t.Fatalf("backspace query=%q valid=%v, want 中", m.histSearch.query, utf8.ValidString(m.histSearch.query))
	}
}

func TestReviewInputHistoryRestoresUnsentDraft(t *testing.T) {
	m := sugModel()
	m.history = []string{"old sent input"}
	m.historyIdx = len(m.history)
	m.textarea.SetValue("unsent draft")
	m.historyPrev()
	m.historyNext()
	if m.textarea.Value() != "unsent draft" {
		t.Fatalf("draft after up/down=%q", m.textarea.Value())
	}
}

func TestReviewQuestionPageScrollMovesLongBody(t *testing.T) {
	m := sugModel()
	m.width, m.height = 24, 12
	request := uiQuestionRequest()
	request.Questions[0].Question = strings.Repeat("Long question details. ", 40)
	m.setQuestion(request)
	before := m.renderQuestionInline()
	m.handleQuestionKey(tea.KeyMsg{Type: tea.KeyPgDown})
	after := m.renderQuestionInline()
	if before == after {
		t.Fatal("PgDown did not change visible question body")
	}
}
