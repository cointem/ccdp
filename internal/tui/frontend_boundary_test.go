package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

func TestFrontendSearchImages(t *testing.T) {
	m, _ := routingTestModel()
	m.history = []string{"inspect screenshot", "ordinary text"}
	m.historyIdx = 2
	img := clipboardPNG(t)
	m.historyImages = map[int][]protocol.InputImage{0: {img}}
	m.startHistorySearch()
	m.histSearch.query = "screenshot"
	m.recomputeHistorySearch()
	m.acceptHistorySearch()
	if m.textarea.Value() != "inspect screenshot" {
		t.Fatal("wrong text")
	}
	if len(m.inputImages) != 1 {
		t.Fatalf("Ctrl+R recalled image prompt with %d images; historyIdx=%d", len(m.inputImages), m.historyIdx)
	}
}
func TestFrontendDragHeader(t *testing.T) {
	m, _ := routingTestModel()
	m = switchRoutingTest(t, m, "child")
	m.width, m.height = 80, 24
	m.confirmedItems = []historyCell{{kind: "assistant", messageID: "a", text: "hello"}}
	m.composeItems()
	m.layout()
	m.viewport.GotoTop()
	h := presentationHeight(m.headerPresentation(), m.width)
	if h < 1 {
		t.Fatal("header missing")
	}
	_, _ = m.handleMouse(tea.MouseMsg{X: 3, Y: h, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	_, _ = m.handleMouse(tea.MouseMsg{X: 3, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	_, _ = m.handleMouse(tea.MouseMsg{X: 3, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease})
}
func TestFrontendWorkspaceProjection(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(t.TempDir(), "sessions")
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.BaseURL = "http://127.0.0.1:1"
	cfg.APIKey = "test"
	a, err := agent.NewWithOptions(&cfg, agent.Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	m := NewWithResume(a, false)
	target := t.TempDir()
	_, cmd := m.runCommand("/cd " + target)
	msg := cmd().(commandReceiptMsg)
	if msg.receipt.Rejected() {
		t.Fatal(msg.receipt.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	for {
		op, e := a.ReadOperation(ctx, msg.receipt.CommandID)
		if e != nil {
			t.Fatal(e)
		}
		if op.Complete {
			if op.Status != "success" {
				t.Fatal(op.Error)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond * 5):
		}
	}
	snap, err := a.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m.applySnapshot(snap)
	m.applyReceipt(protocol.Receipt{CommandID: msg.receipt.CommandID, Status: protocol.ReceiptApplied}, "")
	if got := a.WorkspaceLabel(); got != target {
		t.Fatalf("runtime %s want %s", got, target)
	}
	if m.workspace != target {
		t.Fatalf("runtime workspace=%s but TUI workspace=%s", target, m.workspace)
	}
}
func TestFrontendCollaborationEscapes(t *testing.T) {
	payload := "\x1b]52;c;aGFja2Vk\a"
	for _, m := range []protocol.CollaborationMessage{{Kind: "final_result", Name: payload, Outcome: "success"}, {Kind: "message", Name: "child", Text: payload}} {
		cell := historyCell{kind: "collaboration", messageID: "m", text: protocol.EncodeCollaboration(m)}
		rendered := renderItemWidth(&cell, 80)
		if len(rendered) == 0 {
			t.Fatal("empty")
		}
		for i := 0; i+len(payload) <= len(rendered); i++ {
			if rendered[i:i+len(payload)] == payload {
				t.Fatalf("rendered raw OSC 52 payload from JSON collaboration field")
			}
		}
	}
}
func TestFrontendRejectedInputCanRetryAfterAttachmentAppears(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	cfg.SessionDir = filepath.Join(t.TempDir(), "sessions")
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.BaseURL = "http://127.0.0.1:1"
	cfg.APIKey = "test"
	a, e := agent.NewWithOptions(&cfg, agent.Options{EffectiveConfigFrozen: true})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	m := NewWithResume(a, false)
	m.textarea.SetValue("inspect ![diagram](missing.png)")
	_, first := m.submit()
	msg := first().(commandReceiptMsg)
	if !msg.receipt.Rejected() {
		t.Fatal("expected missing attachment rejection")
	}
	m.applyReceipt(msg.receipt, msg.purpose)
	if e := os.WriteFile(filepath.Join(cfg.Workspace, "missing.png"), clipboardPNG(t).Data, 0600); e != nil {
		t.Fatal(e)
	}
	_, second := m.submit()
	again := second().(commandReceiptMsg)
	if again.receipt.Rejected() {
		t.Fatalf("attachment is now present but retry reused %s and still rejected: %s", again.receipt.CommandID, again.receipt.Error)
	}
}
func TestFrontendTransportRetryPreservesCompleteCommand(t *testing.T) {
	m, d := routingTestModel()
	m.textarea.SetValue("later task")
	_, first := m.submitInput(protocol.InputFollowup)
	_ = first()
	sent := d.clients["root"].submits[0]
	m.restoreFailedSubmission(sent.ID)
	m.snapshot.RunID = "new-viewed-run"
	_, second := m.submit()
	_ = second()
	again := d.clients["root"].submits[1]
	if !reflect.DeepEqual(sent, again) {
		t.Fatalf("same command %s was rebuilt with strategy %s instead of %s", sent.ID, again.Input.Strategy, sent.Input.Strategy)
	}
}
func TestFrontendStructuredHeaderEscapes(t *testing.T) {
	m, _ := routingTestModel()
	q := uiQuestionRequest()
	q.Questions[0].Header = "\x1b[2J"
	m.setQuestion(q)
	if strings.Contains(m.renderQuestionInline(), "\x1b[2J") {
		t.Errorf("validated question header renders raw screen-erase sequence")
	}
	m.question = nil
	row := m.routing.rows[0]
	row.Name = "\x1b]52;c;aGFja2Vk\a"
	m.applyChildUpdate(row)
	if strings.Contains(m.renderAgentShelf(), row.Name) {
		t.Errorf("model-defined child name renders raw OSC52 sequence")
	}
}
func TestFrontendCollaborationFinalFrame(t *testing.T) {
	payload := "\x1b]52;c;aGFja2Vk\a"
	m, _ := routingTestModel()
	m.routing.rows = nil
	m.width, m.height = 80, 24
	m.items = []historyCell{{kind: "collaboration", messageID: "evil", text: protocol.EncodeCollaboration(protocol.CollaborationMessage{Kind: "message", Name: "child", Text: payload + " visible"})}}
	m.layout()
	frame := m.View()
	if strings.Contains(frame, payload) {
		t.Fatalf("final Model.View frame contains attacker OSC52")
	}
}

func TestFrontendHistoryRecallKeepsImageStateAndDraft(t *testing.T) {
	for _, expired := range []bool{false, true} {
		m, _ := routingTestModel()
		image := clipboardPNG(t)
		m.history = []string{""}
		m.historyIdx = 1
		m.historyImages = map[int][]protocol.InputImage{0: {image}}
		if expired {
			m.historyImages = nil
			m.expiredHistoryImages = map[int]bool{0: true}
		}
		m.textarea.SetValue("unsent draft")
		m.inputImages = []protocol.InputImage{image}
		m.startHistorySearch()
		if !strings.Contains(m.renderHistorySearch(), "image input") {
			t.Fatal("pure-image entry mislabeled as no match")
		}
		m.acceptHistorySearch()
		if m.historyIdx != 0 || m.textarea.Value() != "" || m.missingHistoryImages != expired {
			t.Fatal("history entry recalled inconsistently")
		}
		if !expired && len(m.inputImages) != 1 {
			t.Fatal("image-only history lost its image")
		}
		if expired {
			if _, cmd := m.submit(); cmd != nil {
				t.Fatal("expired image history was sent without attachment")
			}
		}
		m.historyNext()
		if m.textarea.Value() != "unsent draft" || len(m.inputImages) != 1 || m.missingHistoryImages {
			t.Fatal("recall lost original draft or image state")
		}
	}
}

func TestFrontendWorkspaceStateRefreshesPathFeatures(t *testing.T) {
	oldDir, newDir := t.TempDir(), t.TempDir()
	if e := os.WriteFile(filepath.Join(oldDir, "old.txt"), []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(newDir, "new.txt"), []byte("new"), 0600); e != nil {
		t.Fatal(e)
	}
	commandDir := filepath.Join(newDir, ".ccdp", "commands")
	if e := os.MkdirAll(commandDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(commandDir, "new-command.md"), []byte("new command"), 0600); e != nil {
		t.Fatal(e)
	}
	m, _ := routingTestModel()
	m.workspace = oldDir
	m.textarea.SetValue("@")
	previousGeneration := m.reportGeneration
	view := m.snapshot
	view.Settings.Workspace = newDir
	state := view.State()
	view.ApplyState(state)
	m.applySnapshot(view)
	if m.workspace != newDir || m.reportGeneration == previousGeneration || m.findCustomCommand("new-command") == nil {
		t.Fatal("workspace projection did not refresh custom commands")
	}
	if m.fileMention == nil {
		t.Fatal("workspace change did not refresh path completion")
	}
	for _, candidate := range m.fileMention.items {
		if candidate.label == "old.txt" {
			t.Fatal("old workspace completion survived")
		}
	}
	if got := terminalLinkTarget("new.txt", m.workspace); !strings.Contains(got, newDir) {
		t.Fatal("relative link uses old workspace")
	}
}

func TestFrontendSelectionOutsideBodyClamps(t *testing.T) {
	rows := []string{"first", "中文 last"}
	if got := selectionText(rows, selPoint{Row: 0, Col: 2}, selPoint{Row: -100, Col: 9}); got != "fi" {
		t.Fatalf("upward clamp=%q", got)
	}
	if got := selectionText(rows, selPoint{Row: 0, Col: 0}, selPoint{Row: 100000, Col: 9}); got != "first\n中文 last" {
		t.Fatalf("downward clamp=%q", got)
	}
	if selectionText(nil, selPoint{}, selPoint{Row: -1}) != "" {
		t.Fatal("empty selection grid")
	}
}

func TestFrontendChildWorkspaceIsIndependent(t *testing.T) {
	m, d := routingTestModel()
	rootDir, childDir := t.TempDir(), t.TempDir()
	d.clients["root"].snapshot.Settings.Workspace = rootDir
	d.clients["child"].snapshot.Settings.Workspace = childDir
	m.applySnapshot(d.clients["root"].snapshot)
	m = switchRoutingTest(t, m, "child")
	if m.workspace != childDir {
		t.Fatal("child inherited root workspace")
	}
	m = switchRoutingTest(t, m, "root")
	if m.workspace != rootDir {
		t.Fatal("returning from child changed root workspace")
	}
}
