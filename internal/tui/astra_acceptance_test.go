package tui

// Independent acceptance fixtures for the complete TUI experience contract.
// They exercise the real renderer and update paths without a live provider.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func astraModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := NewWithClient(nil, "/workspace/ccdp", false)
	t.Cleanup(m.watchCancel)
	view := protocolSnapshot("acceptance-root")
	view.Settings.Model.Model = "deepseek-chat"
	view.Settings.ReasoningEffort = "high"
	view.Settings.ContextWindow = 200000
	view.ContextUsedTokens = 12345
	m.applySnapshot(view)
	m.width, m.height = width, height
	m.items = []historyCell{{kind: "user", text: "检查布局"}, {kind: "assistant", text: "Recent context"}}
	m.textarea.SetValue("draft")
	m.layout()
	return m
}

func TestAstraAcceptanceFrames(t *testing.T) {
	originalProfile, originalDark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetColorProfile(originalProfile); lipgloss.SetHasDarkBackground(originalDark) })
	frames := map[string]string{}
	profiles := []struct {
		name    string
		profile termenv.Profile
		dark    bool
	}{{"dark", termenv.TrueColor, true}, {"light", termenv.TrueColor, false}, {"no-color", termenv.Ascii, true}, {"ansi256", termenv.ANSI256, true}}
	for _, profile := range profiles {
		lipgloss.SetColorProfile(profile.profile)
		lipgloss.SetHasDarkBackground(profile.dark)
		for _, size := range [][2]int{{24, 12}, {40, 16}, {80, 24}, {120, 40}} {
			for _, scene := range []string{"conversation", "markdown", "tool", "approval", "plan", "question", "picker", "reader"} {
				name := fmt.Sprintf("%s/%dx%d/%s", profile.name, size[0], size[1], scene)
				t.Run(name, func(t *testing.T) {
					m := astraModel(t, size[0], size[1])
					switch scene {
					case "markdown":
						m.items[1].text = "## Report\n\n**Chinese 中文** e\u0301 👨‍👩‍👧‍👦\n\n| Name | Value |\n| --- | --- |\n| alpha | a long table value |\n\n```go\n    fmt.Println(\"hello\")\n```"
					case "tool":
						m.applyTranscript([]protocol.TranscriptItem{{ID: "tool-1", Kind: "tool", Tool: "Bash", CallID: "call-1", Args: json.RawMessage(`{"command":"go test ./..."}`), Status: "running", Text: "line one\nline two\nline three"}})
						m.busy = true
					case "approval", "plan":
						tool := "Bash"
						if scene == "plan" {
							tool = "Plan"
						}
						m.approval = &approvalPrompt{ID: "approval-1", Tool: tool, Command: strings.Repeat("review this action\n", 20), Reason: "Needs approval"}
					case "question":
						m.setQuestion(uiQuestionRequest())
					case "reader":
						m.openTranscriptReader()
					case "picker":
						m.picker = &selectorPanel{Selector: Selector{Title: "Model", Options: []SelectorOption{{ID: "deepseek-chat", Label: "deepseek-chat"}, {ID: "another-model", Label: "another-model"}}}}
					}
					m.layout()
					frame := m.View()
					plain := sanitizeANSI(frame)
					frames[name] = frame
					if w, h := lipgloss.Width(frame), lipgloss.Height(frame); w > size[0] || h > size[1] {
						t.Errorf("frame %dx%d exceeds terminal %dx%d:\n%s", w, h, size[0], size[1], plain)
					}
					for _, required := range []string{"high", "manual", "12.3k/200k"} {
						if !strings.Contains(plain, required) {
							t.Errorf("missing always-visible %q:\n%s", required, plain)
						}
					}
					if !strings.Contains(plain, "deepseek") {
						t.Errorf("model is missing or unrecognizable:\n%s", plain)
					}
					if size[0] >= 60 {
						compact := false
						for _, line := range strings.Split(plain, "\n") {
							compact = compact || strings.Contains(line, "deepseek-chat") && strings.Contains(line, "high") && strings.Contains(line, "12.3k/200k")
						}
						if !compact {
							t.Errorf("mandatory status triplet did not share one row despite sufficient width:\n%s", plain)
						}
					}
					if strings.Contains(plain, "ctx:") || strings.Contains(plain, "≈") || strings.Contains(plain, "6.2%") {
						t.Errorf("context includes unwanted labels:\n%s", plain)
					}
					if (scene == "approval" || scene == "plan" || scene == "question") && size[0] >= 40 && !strings.Contains(sanitizeANSI(m.viewport.View()), "Recent context") {
						t.Errorf("decision hides recent conversation:\n%s", plain)
					}
					if scene == "plan" && (strings.Contains(plain, "always") || strings.Contains(plain, "never") || strings.Contains(plain, "[a]")) {
						t.Errorf("plan exposes tool-rule approval options:\n%s", plain)
					}
					if (scene == "approval" || scene == "plan" || scene == "question" || scene == "picker") && strings.Contains(strings.ToLower(plain), "enter send") {
						t.Errorf("active decision/selector advertises sending despite owning Enter:\n%s", plain)
					}
				})
			}
		}
	}
	if path := os.Getenv("CCDP_ACCEPTANCE_FRAMES"); path != "" {
		data, err := json.MarshalIndent(frames, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAstraAcceptanceDraftAndInputHistory(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.textarea.SetValue("draft 中文\nsecond line")
	m.inputImages = []protocol.InputImage{clipboardPNG(t)}
	wantText, wantImages := m.textarea.Value(), append([]protocol.InputImage(nil), m.inputImages...)
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.textarea.Value() != wantText || !reflect.DeepEqual(m.inputImages, wantImages) {
		t.Fatal("Esc destroyed draft text or image attachments")
	}
	m.inputImages = nil
	m.textarea.SetValue("")
	m.history, m.historyIdx = []string{"first request", "second request"}, 2
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.textarea.Value() != "second request" {
		t.Fatalf("Up did not recall input history: %q", m.textarea.Value())
	}
}

func TestAstraAcceptanceNavigationNeverErasesNativeHistory(t *testing.T) {
	var terminal bytes.Buffer
	reset := &resetAgentTerminal{}
	reset.SetStdout(&terminal)
	if err := reset.Run(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(terminal.String(), "\x1b[3J") {
		t.Fatal("navigation still erases native terminal scrollback")
	}
	m, directory := routingTestModel()
	t.Cleanup(m.watchCancel)
	directory.clients["root"].snapshot.Settings.ContextWindow = 200000
	directory.clients["root"].snapshot.ContextUsedTokens = 12345
	directory.clients["child"].snapshot.Settings.Model.Model = "child-model"
	directory.clients["child"].snapshot.Settings.ReasoningEffort = "low"
	directory.clients["child"].snapshot.Settings.ContextWindow = 64000
	directory.clients["child"].snapshot.ContextUsedTokens = 1000
	m.applySnapshot(directory.clients["root"].snapshot)
	m.textarea.SetValue("root draft")
	m = switchRoutingTest(t, m, "child")
	childFrame := sanitizeANSI(m.View())
	if !strings.Contains(childFrame, "child-model") || !strings.Contains(childFrame, "1k/64k") || strings.Contains(childFrame, "12.3k/200k") {
		t.Errorf("child footer uses wrong session settings/context:\n%s", childFrame)
	}
	m.textarea.SetValue("child draft")
	m = switchRoutingTest(t, m, "root")
	if m.textarea.Value() != "root draft" {
		t.Fatal("return did not restore root draft")
	}
	if directory.clients["root"].submitCount()+directory.clients["child"].submitCount() != 0 {
		t.Fatal("navigation submitted a runtime operation")
	}
}

type astraTerminalCommandModel struct {
	command tea.Cmd
	follow  []tea.Cmd
}
type astraTerminalTimeout struct{}

func (m astraTerminalCommandModel) Init() tea.Cmd {
	return tea.Batch(m.command, tea.Tick(time.Second, func(time.Time) tea.Msg { return astraTerminalTimeout{} }))
}
func (m astraTerminalCommandModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message.(type) {
	case inlinePrintedMsg, agentViewResetMsg:
		if len(m.follow) > 0 {
			command := m.follow[0]
			m.follow = m.follow[1:]
			return m, command
		}
		return m, tea.Quit
	case astraTerminalTimeout:
		return m, tea.Quit
	}
	return m, nil
}
func (astraTerminalCommandModel) View() string { return "acceptance transition" }

func astraTerminalCommandOutput(t *testing.T, command tea.Cmd) string {
	return astraTerminalCommandsOutput(t, command)
}

func astraTerminalCommandsOutput(t *testing.T, commands ...tea.Cmd) string {
	t.Helper()
	if len(commands) == 0 {
		return ""
	}
	var output bytes.Buffer
	program := tea.NewProgram(astraTerminalCommandModel{command: commands[0], follow: commands[1:]}, tea.WithInput(strings.NewReader("")), tea.WithOutput(&output), tea.WithoutSignalHandler())
	if _, err := program.Run(); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestAstraAcceptanceAgentAlternateScreenAndPrintIdentity(t *testing.T) {
	m, directory := routingTestModel()
	t.Cleanup(m.watchCancel)
	root := protocolSnapshot("root", protocol.MessageView{ID: "root-message", Role: "assistant", Content: "ROOT ALREADY PRINTED"})
	directory.clients["root"].snapshot = root
	m.applySnapshot(root)
	m.inline.prime(m.items)
	for index, item := range m.items {
		m.inline.mark(item, index)
	}
	rootItem := m.items[0]
	opened := m.openAgentView("child")()
	next, transition := m.Update(opened)
	m = modelValue(t, next)
	if transition == nil {
		t.Fatal("child navigation emitted no surface transition")
	}
	enterTransition := transition
	next, _ = m.Update(agentViewResetMsg{generation: m.routing.generation})
	m = modelValue(t, next)
	// A child view must stay managed; no child message belongs in root scrollback.
	m.items = append(m.items, historyCell{kind: "assistant", messageID: "child-message", text: "CHILD OUTPUT"})
	if command := planTestHistory(&m); command != nil {
		t.Error("child detail queued ordinary native-scrollback output")
	}
	opened = m.openAgentView("root")()
	next, transition = m.Update(opened)
	m = modelValue(t, next)
	// Run both transitions in one renderer: ExitAltScreen is meaningful only
	// after that renderer entered the child surface.
	output := astraTerminalCommandsOutput(t, enterTransition, transition)
	enterAt, exitAt := strings.Index(output, "\x1b[?1049h"), strings.Index(output, "\x1b[?1049l")
	if enterAt < 0 || exitAt < enterAt {
		t.Fatalf("missing ordered child enter/root return transitions: %q", output)
	}
	if strings.Count(output, "\x1b[?1049h") != 1 {
		t.Errorf("navigation entered alternate screen more than once: %q", output)
	}
	if strings.Contains(output[exitAt:], "\x1b[2J") || strings.Contains(output, "\x1b[3J") {
		t.Errorf("root screen/history was cleared during return: %q", output)
	}
	next, _ = m.Update(agentViewResetMsg{generation: m.routing.generation})
	m = modelValue(t, next)
	if !m.inline.seen(rootItem, 0) {
		t.Fatal("return forgot root print identity and will repeat native history")
	}
	if command := planTestHistory(&m); command != nil {
		t.Error("return queued already-printed root conversation again")
	}
}

func TestAstraAcceptanceMarkdownIsScopedAndSourcePreserved(t *testing.T) {
	source := "## Heading\n\n**bold** and `code`\n\n```go\n    x := 1\n```"
	item := historyCell{kind: "assistant", text: source}
	got := sanitizeANSI(renderItemWidth(&item, 80))
	if strings.Contains(got, "## Heading") || strings.Contains(got, "**bold**") || strings.Contains(got, "```go") {
		t.Fatalf("assistant still displays Markdown syntax:\n%s", got)
	}
	if !strings.Contains(got, "    x := 1") {
		t.Fatalf("code indentation was lost:\n%s", got)
	}
	if item.text != source {
		t.Fatal("Markdown rendering changed source text")
	}
	for _, kind := range []string{"user", "system", "command"} {
		plain := historyCell{kind: kind, text: "## literal **output**"}
		if rendered := sanitizeANSI(renderItemWidth(&plain, 80)); !strings.Contains(rendered, plain.text) {
			t.Errorf("%s was incorrectly parsed as Markdown: %q", kind, rendered)
		}
	}
	// Equal-length replacement is a real snapshot correction, not a cache hit.
	item = historyCell{kind: "assistant", text: "OLD"}
	_ = renderItemWidth(&item, 80)
	item.text = "NEW"
	if rendered := sanitizeANSI(renderItemWidth(&item, 80)); !strings.Contains(rendered, "NEW") || strings.Contains(rendered, "OLD") {
		t.Fatalf("same-length correction remained stale: %q", rendered)
	}
	for _, source := range []string{"```go\nLAST_CODE_LINE", "```go\nfirst\nLAST_CODE_LINE", "```"} {
		item := historyCell{kind: "assistant", text: source}
		got := sanitizeANSI(renderItemWidth(&item, 80))
		if strings.Contains(source, "LAST_CODE_LINE") && !strings.Contains(got, "LAST_CODE_LINE") {
			t.Errorf("unclosed code fence lost the most recent line: source=%q rendered=%q", source, got)
		}
	}
	identifier := historyCell{kind: "assistant", text: "Inspect foo_bar_baz and STREAM_A_01."}
	if rendered := sanitizeANSI(renderItemWidth(&identifier, 80)); !strings.Contains(rendered, "foo_bar_baz") || !strings.Contains(rendered, "STREAM_A_01") {
		t.Errorf("Markdown parser removed intraword underscores from identifiers: %q", rendered)
	}
}

func TestAstraAcceptanceConfirmedFooterAndUnknownContext(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.layout()
	plain := sanitizeANSI(m.View())
	if !strings.Contains(plain, "deepseek-chat") || !strings.Contains(plain, "high") || !strings.Contains(plain, "12.3k/200k") {
		t.Fatalf("confirmed footer missing authoritative settings:\n%s", plain)
	}
	m.snapshot.Settings.ContextWindow = 0
	m.layout()
	if plain = sanitizeANSI(m.View()); !strings.Contains(plain, "12.3k/—") {
		t.Fatalf("unknown capacity disappeared:\n%s", plain)
	}
	m.snapshot.Settings.ContextWindow = 200000
	m.snapshot.ContextUsedTokens = 250000
	m.layout()
	if plain = sanitizeANSI(m.View()); !strings.Contains(plain, "250k/200k") {
		t.Fatalf("over-capacity estimate was clipped:\n%s", plain)
	}
}

func TestAstraAcceptanceQuestionBackRetainsAnswer(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.setQuestion(uiQuestionRequest())
	m.handleQuestionKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleQuestionKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.question.index != 1 || !reflect.DeepEqual(m.question.answers[0].Selected, []string{"B"}) {
		t.Fatal("fixture did not record B")
	}
	m.handleQuestionKey(tea.KeyMsg{Type: tea.KeyCtrlP})
	m.handleQuestionKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !reflect.DeepEqual(m.question.answers[0].Selected, []string{"B"}) {
		t.Fatalf("returning to answered question silently changed answer: %#v", m.question.answers[0])
	}
}

func TestAstraAcceptanceToolSummaryDoesNotMutateSource(t *testing.T) {
	for _, tool := range []string{"Read", "Search", "Bash", "Edit", "custom_tool"} {
		t.Run(tool, func(t *testing.T) {
			m := astraModel(t, 80, 24)
			output := "FIRST OUTPUT\n" + strings.Repeat("intermediate output\n", 20) + "FINAL OUTPUT"
			m.applyTranscript([]protocol.TranscriptItem{{ID: "tool-summary", Kind: "tool", Tool: tool, CallID: "summary-call", Args: json.RawMessage(`{"file_path":"src/main.go","command":"go test ./...","pattern":"needle"}`), Status: "success", Text: output}})
			if len(m.items) != 1 {
				t.Fatalf("tool projected into %d conversation entries", len(m.items))
			}
			item := &m.items[0]
			source := item.text
			rendered := sanitizeANSI(renderItemWidth(item, 80))
			budget, label := 3, tool
			if tool == "Bash" {
				budget, label = 6, "Ran"
			}
			if h := lipgloss.Height(rendered); h > budget {
				t.Errorf("successful %s occupies %d lines, want <=%d:\n%s", tool, h, budget, rendered)
			}
			if !strings.Contains(rendered, label) {
				t.Errorf("tool name is unreadable: %q", rendered)
			}
			// Rendering must be a projection and leave its source intact.
			if item.text != source {
				t.Fatal("tool rendering mutated source output")
			}
		})
	}
}

func TestAstraAcceptanceUnifiedDiffIdentityAndLineNumbers(t *testing.T) {
	source := "--- a/main.go\n+++ b/main.go\n@@ -10,2 +20,2 @@\n-old_value\n+new_value\n context_value\n\\ No newline at end of file"
	view := sanitizeANSI(renderDiff(source, 80))
	if !strings.Contains(strings.Split(view, "\n")[0], "main.go") {
		t.Errorf("unified diff did not show file identity in its heading:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		fields := strings.Fields(line)
		if strings.Contains(line, "old_value") && (len(fields) < 4 || fields[0] != "10" || fields[1] != "-" || fields[2] != "-") {
			t.Errorf("deleted line must have old line 10 and no new line: %q", line)
		}
		if strings.Contains(line, "new_value") && (len(fields) < 4 || fields[0] != "-" || fields[1] != "20" || fields[2] != "+") {
			t.Errorf("added line must have new line 20 and no old line: %q", line)
		}
	}
	multiple := source + "\n--- a/second.go\n+++ b/second.go\n@@ -1 +1 @@\n-before\n+after"
	view = sanitizeANSI(renderDiff(multiple, 80))
	var headings []string
	for _, line := range strings.Split(view, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "✎") {
			headings = append(headings, line)
		}
	}
	if len(headings) != 2 || !strings.Contains(headings[0], "main.go") || !strings.Contains(headings[1], "second.go") {
		t.Errorf("standard multi-file unified diff collapsed file boundaries: headings=%q", headings)
	}
}

func TestAstraAcceptanceStreamFreezesOnlyStableMarkdown(t *testing.T) {
	for _, source := range []string{
		"| Alpha | Beta |\n",
		"- first item\n- second item",
		"**" + strings.Repeat("unfinished emphasis ", 180),
		"```go\n" + strings.Repeat("    code line\n", 100),
	} {
		if end := markdownStablePrefixEnd(source, 0, 40); end != 0 {
			t.Errorf("unfinished Markdown was frozen before its source block was stable: committed=%d source prefix=%q", end, source[:min(len(source), 60)])
		}
	}
	stable := "A complete paragraph.\n\n"
	source := stable + "| Alpha | Beta |\n"
	if end := markdownStablePrefixEnd(source, 0, 40); end != len(stable) {
		t.Errorf("stable paragraph plus incomplete table committed through %d, want %d", end, len(stable))
	}
}

func TestAstraAcceptanceLongUnclosedFenceShowsLatestSource(t *testing.T) {
	m := astraModel(t, 80, 24)
	source := "```go\n" + strings.Repeat("    source line\n", 10000) + "    LAST_CODE_LINE"
	m.items = []historyCell{{kind: "assistant", text: source}}
	m.busy, m.streaming, m.turnDone = true, true, false
	m.layout()
	if frame := sanitizeANSI(m.View()); !strings.Contains(frame, "LAST_CODE_LINE") {
		t.Fatalf("huge unclosed code fence hides newest output:\n%s", frame)
	}
	if m.items[0].text != source {
		t.Fatal("bounded rendering mutated full streamed source")
	}
}

func astraFlushRoutedTranscript(t *testing.T, m *Model) string {
	t.Helper()
	var output bytes.Buffer
	m.terminal = NewTerminalHost(&output)
	queued := m.flushInline()
	if queued == nil {
		return ""
	}
	batch := queued().(inlinePrintMsg)
	next, printable := m.Update(batch)
	*m = modelValue(t, next)
	if printable == nil {
		t.Fatal("batch never reached terminal")
	}
	program := tea.NewProgram(astraTerminalCommandModel{command: printable}, tea.WithInput(nil), tea.WithOutput(m.terminal), tea.WithoutSignalHandler())
	if _, err := program.Run(); err != nil {
		t.Fatal(err)
	}
	next, _ = m.Update(inlinePrintedMsg{generation: m.routing.generation, batch: batch.batch})
	*m = modelValue(t, next)
	return output.String()
}

func TestAstraAcceptanceTypedStreamPrintsEverySourceMarkerOnce(t *testing.T) {
	m, _ := routingTestModel()
	t.Cleanup(m.watchCancel)
	m.items, m.confirmedItems = nil, nil
	m.inline.forgetAll()
	m.inline.prime(nil)
	m.inline.showBaseline = false
	m.busy, m.streaming, m.turnDone = true, true, false
	live := protocol.TranscriptItem{ID: "stream:turn-1:1", TurnID: "turn-1", Kind: "assistant", Status: "streaming", Text: "STREAMMARKERA01\n\n"}
	m.handleProtocolEvent(protocol.EventView{SessionID: "root", Kind: protocol.EventStream, TurnID: "turn-1", Text: live.Text, Transcript: &live})
	output := astraFlushRoutedTranscript(t, &m)
	delta := strings.Repeat("a stable complete paragraph\n\n", 100) + "STREAMMARKERB02\n\n"
	live.Text += delta
	m.handleProtocolEvent(protocol.EventView{SessionID: "root", Kind: protocol.EventStream, TurnID: "turn-1", Text: delta, Transcript: &live})
	output += astraFlushRoutedTranscript(t, &m)
	m.width = 40
	m.layout()
	output += astraFlushRoutedTranscript(t, &m)
	delta = "STREAMMARKERC03"
	live.Text += delta
	m.handleProtocolEvent(protocol.EventView{SessionID: "root", Kind: protocol.EventStream, TurnID: "turn-1", Text: delta, Transcript: &live})
	output += astraFlushRoutedTranscript(t, &m)
	final := live
	final.ID, final.PreviousID, final.Status = "committed-turn-1", live.ID, "completed"
	m.applyTranscript([]protocol.TranscriptItem{final})
	m.turnDone, m.busy = true, false
	output += astraFlushRoutedTranscript(t, &m)
	for _, marker := range []string{"STREAMMARKERA01", "STREAMMARKERB02", "STREAMMARKERC03"} {
		if count := strings.Count(output, marker); count != 1 {
			t.Errorf("typed stream marker %s printed %d times, want exactly once", marker, count)
		}
	}
}

func TestAstraAcceptanceLateReceiptCannotReplaceNewerOperation(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.client = &recordingClient{snapshot: m.snapshot}
	m.submitCommand(protocol.Command{ID: "older", Type: protocol.CommandQuery}, "older setting applied")
	m.submitCommand(protocol.Command{ID: "newer", Type: protocol.CommandQuery}, "newer setting applied")
	m.applyReceipt(protocol.Receipt{CommandID: "newer", SessionID: "acceptance-root", Status: protocol.ReceiptApplied}, "newer setting applied")
	m.applyReceipt(protocol.Receipt{CommandID: "older", SessionID: "acceptance-root", Status: protocol.ReceiptApplied}, "older setting applied")
	if active := m.Activity(); active.CommandID != "newer" {
		t.Errorf("late old receipt took ownership from newer feedback: %+v", active)
	}
	m.applyReceipt(protocol.Receipt{CommandID: "newer", SessionID: "acceptance-root", Status: protocol.ReceiptScheduled}, "newer setting queued")
	for _, operation := range m.Operations() {
		if operation.CommandID == "newer" && operation.Active() {
			t.Errorf("late scheduled receipt regressed completed operation to pending: %+v", operation)
		}
	}
}

func TestAstraAcceptanceReaderLongEntryCopyAndReturn(t *testing.T) {
	m := astraModel(t, 80, 24)
	source := "## Source heading\n\n" + strings.Repeat("source line\n", 40) + "LAST SOURCE LINE"
	m.snapshot.Transcript = []protocol.TranscriptItem{{ID: "long-message", Kind: "assistant", Text: source}}
	clipboard := &captureClipboard{}
	m.clipboard = clipboard
	wantReports := len(m.reports)
	if cmd := m.openTranscriptReader(); cmd == nil {
		t.Fatal("reader did not enter its detail surface")
	}
	first := m.readerBody(76, 6)
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyPgDown})
	second := m.readerBody(76, 6)
	if first == second {
		t.Fatal("single long transcript entry cannot scroll past its first page")
	}
	if strings.Contains(sanitizeANSI(first), "## Source heading") {
		t.Error("formatted transcript reader still shows raw Markdown heading markers")
	}
	_, copy := m.handleReaderKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if copy == nil {
		t.Fatal("reader copy command unavailable")
	}
	copy()
	if clipboard.text != source {
		t.Fatalf("reader copied decorations or lost source: copied %d bytes, want %d", len(clipboard.text), len(source))
	}
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.reader != nil || m.textarea.Value() != "draft" || len(m.reports) != wantReports {
		t.Fatal("reader return changed draft or polluted conversation")
	}
}

func TestAstraAcceptanceOutputSearchAndDisplaySanitization(t *testing.T) {
	m := astraModel(t, 80, 24)
	source := strings.Repeat("filler line\n", 25) + "needle target\n\x1b[3Jafter control"
	m.openOutputReader(agentOutputMsg{sessionID: m.sessionID, itemID: "output-1", page: protocol.OutputPage{Text: source, Next: int64(len(source)), Total: int64(len(source))}})
	_ = m.readerBody(76, 6)
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("needle")})
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyEnter})
	if body := m.readerBody(76, 6); !strings.Contains(body, "needle target") {
		t.Errorf("search did not scroll to offscreen output match: %q", body)
	}
	m.handleReaderKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if body := m.readerBody(76, 100); strings.Contains(body, "\x1b[3J") {
		t.Fatal("raw reader display emitted stored terminal control sequence")
	}
}

func TestAstraAcceptanceClosedReaderRejectsLatePage(t *testing.T) {
	m := astraModel(t, 80, 24)
	m.readerSeq, m.readerRequest = 7, 7
	msg := agentOutputMsg{sessionID: m.sessionID, itemID: "saved-output", request: 7, page: protocol.OutputPage{Text: "old output"}}
	m.openOutputReader(msg)
	m.closeReader()
	m.openOutputReader(msg)
	if m.reader != nil {
		t.Fatal("late page reopened a reader after Esc")
	}
}

func TestAstraAcceptanceNarrowChildDecisionKeepsTarget(t *testing.T) {
	for _, decision := range []string{"approval", "question"} {
		t.Run(decision, func(t *testing.T) {
			m := astraModel(t, 24, 12)
			m.sessionID, m.snapshot.SessionID = "child-target", "child-target"
			m.routing = &sessionRouting{rootID: "root"}
			if decision == "approval" {
				m.approval = &approvalPrompt{ID: "approval", Tool: "Bash", Command: strings.Repeat("review command\n", 20), Reason: "permission required"}
			} else {
				m.setQuestion(uiQuestionRequest())
			}
			m.layout()
			if frame := sanitizeANSI(m.headerPresentation()); !strings.Contains(frame, "child-target") {
				t.Fatalf("narrow child decision hides its current input/approval target:\n%s", frame)
			}
		})
	}
}

func TestAstraAcceptanceViewportTopAnchorMatchesVisibleBody(t *testing.T) {
	m := astraModel(t, 40, 16)
	m.sessionID, m.snapshot.SessionID = "child-target", "child-target"
	m.routing = &sessionRouting{rootID: "root"}
	m.inlineMode, m.followOutput = false, false
	m.items = []historyCell{{kind: "system", text: "VISIBLE ANCHOR\n" + strings.Repeat("later history row\n", 50)}}
	m.layout()
	m.viewport.GotoTop()
	if frame := sanitizeANSI(m.View()); !strings.Contains(frame, "VISIBLE ANCHOR") {
		t.Fatalf("viewport offset zero was clipped by unmeasured chrome:\n%s", frame)
	}
}

func TestAstraAcceptanceDynamicChromeResizesThroughUpdate(t *testing.T) {
	newModel := func(t *testing.T) Model {
		m := astraModel(t, 40, 16)
		m.inlineMode, m.followOutput = false, false
		m.items = []historyCell{{kind: "system", text: "DYNAMIC ANCHOR\n" + strings.Repeat("later row\n", 40)}}
		m.layout()
		m.viewport.GotoTop()
		return m
	}
	assertAnchor := func(t *testing.T, m Model) {
		t.Helper()
		if frame := sanitizeANSI(m.View()); !strings.Contains(frame, "DYNAMIC ANCHOR") {
			t.Fatalf("dynamic chrome clipped viewport offset-zero anchor:\n%s", frame)
		}
	}
	t.Run("receipt and expiry", func(t *testing.T) {
		m := newModel(t)
		idleHeight := m.viewport.Height
		m.registerOperation(protocol.Command{ID: "notice-op", SessionID: protocol.SessionID(m.sessionID), Type: protocol.CommandQuery}, "settings applied")
		next, _ := m.Update(commandReceiptMsg{sessionID: protocol.SessionID(m.sessionID), purpose: "settings applied", receipt: protocol.Receipt{CommandID: "notice-op", SessionID: protocol.SessionID(m.sessionID), Status: protocol.ReceiptApplied}})
		m = modelValue(t, next)
		assertAnchor(t, m)
		// A command receipt is an input-lane hint. It must stay visible without
		// reserving a row from the transcript, so an adjustment never reflows the
		// output above the composer.
		if m.viewport.Height != idleHeight {
			t.Fatalf("receipt hint reflowed the transcript: height %d, want %d", m.viewport.Height, idleHeight)
		}
		if frame := sanitizeANSI(m.View()); !strings.Contains(frame, "settings applied") {
			t.Fatalf("receipt hint missing from the composer lane:\n%s", frame)
		}
		next, _ = m.Update(noticeTickMsg{at: now().Add(10 * time.Second)})
		m = modelValue(t, next)
		assertAnchor(t, m)
		if m.viewport.Height != idleHeight {
			t.Fatalf("expired notice left stale body height %d, want %d", m.viewport.Height, idleHeight)
		}
		if frame := sanitizeANSI(m.View()); strings.Contains(frame, "settings applied") {
			t.Fatalf("expired receipt hint stayed in the composer lane:\n%s", frame)
		}
	})
	t.Run("folded paste", func(t *testing.T) {
		m := newModel(t)
		idleHeight := m.viewport.Height
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Repeat("pasted line\n", 20)), Paste: true})
		m = modelValue(t, next)
		assertAnchor(t, m)
		// Collapsing replaces the one-line composer; expanding creates real
		// extra rows and must resize the returned pointer model immediately.
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
		m = modelValue(t, next)
		assertAnchor(t, m)
		if m.viewport.Height >= idleHeight {
			t.Fatal("expanded paste did not reserve its visible composer rows")
		}
	})
}

func BenchmarkAstraAcceptanceActiveTail(b *testing.B) {
	for _, historySize := range []int{100, 10000} {
		b.Run(fmt.Sprintf("history_%d", historySize), func(b *testing.B) {
			m := NewWithClient(nil, "/workspace/ccdp", false)
			b.Cleanup(m.watchCancel)
			m.width, m.height = 80, 24
			m.modelName = "deepseek-chat"
			m.inlineMode, m.followOutput, m.busy, m.streaming = true, true, true, true
			for i := 0; i < historySize; i++ {
				item := historyCell{kind: "assistant", messageID: fmt.Sprintf("old-%d", i), text: "A completed line of conversation."}
				m.items = append(m.items, item)
				m.inline.mark(item, i)
			}
			m.items = append(m.items, historyCell{kind: "assistant", text: "An active tail still receiving output."})
			m.layout()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

func BenchmarkAstraAcceptanceUnclosedFence(b *testing.B) {
	for _, lineCount := range []int{100, 10000} {
		b.Run(fmt.Sprintf("lines_%d", lineCount), func(b *testing.B) {
			m := NewWithClient(nil, "/workspace/ccdp", false)
			b.Cleanup(m.watchCancel)
			m.width, m.height = 80, 24
			m.modelName = "deepseek-chat"
			m.inlineMode, m.followOutput, m.busy, m.streaming = true, true, true, true
			m.items = []historyCell{{kind: "assistant", text: "```go\n" + strings.Repeat("    fmt.Println(\"streaming line\")\n", lineCount)}}
			m.layout()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}
