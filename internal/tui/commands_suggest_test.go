package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ccdp/internal/commands"
	"ccdp/internal/permissions"
	"ccdp/internal/protocol"
)

// sugModel builds a bare Model with just enough state for the suggestion logic.
func sugModel() *Model {
	// Production textarea construction so key bindings match the real TUI.
	ta := newTextarea()
	ta.SetWidth(40)
	snapshot := protocol.SessionView{SessionID: "test-session",
		Settings: protocol.SettingsSnapshot{Model: protocol.ModelBinding{Model: "test-model"},
			Permission:    protocol.PermissionPolicy{Mode: string(permissions.ModeDefault)},
			Sandbox:       protocol.SandboxPolicy{},
			ContextWindow: 100000},
		Catalog: protocol.CatalogSnapshot{Providers: []protocol.ProviderSnapshot{{ID: "test", Models: []string{"test-model", "test-model-2"}}}}}
	client := &recordingClient{snapshot: snapshot}
	return &Model{composerState: composerState{textarea: ta}, client: client, snapshot: snapshot, hasSnapshot: true,
		sessionID: "test-session", modelName: "test-model", mode: permissions.ModeDefault,
		workspace: "", followOutput: true}
}

func typeKey(m *Model, s string) {
	m.textarea, _ = m.textarea.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	m.refreshCmdSuggest()
}

func TestCommandNamesParsed(t *testing.T) {
	if len(commandNames) < 20 {
		t.Errorf("expected a substantial command list, got %d", len(commandNames))
	}
	for _, want := range []string{"help", "clear", "memory", "github", "pr-comments", "commit-push-pr", "quit", "exit", "config", "permissions"} {
		found := false
		for _, n := range commandNames {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("command %q missing from commandNames", want)
		}
	}
	// No stray punctuation from usage lines like "/quit, /exit".
	for _, n := range commandNames {
		if strings.ContainsAny(n, ",|/") {
			t.Errorf("command name has junk punctuation: %q", n)
		}
	}
}

func TestRefreshCmdSuggest(t *testing.T) {
	m := sugModel()

	// Plain text: no suggestions.
	typeKey(m, "fix the bug")
	if len(m.cmdSug) != 0 {
		t.Errorf("plain text opened suggestions: %v", m.cmdSug)
	}

	// "/" alone keeps the complete result set; rendering applies the cap.
	m.textarea.SetValue("/")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != len(commandNames) {
		t.Errorf("'/' should retain all %d commands, got %d", len(commandNames), len(m.cmdSug))
	}
	if m.cmdSugIdx != 0 {
		t.Errorf("selection should start at 0, got %d", m.cmdSugIdx)
	}

	// "/mem" filters to the memory command (plus none other).
	m.textarea.SetValue("/mem")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != 1 || m.cmdSug[0] != "memory" {
		t.Errorf("'/mem' should suggest [memory], got %v", m.cmdSug)
	}

	// Exact full command: popup closes.
	m.textarea.SetValue("/memory")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != 0 {
		t.Errorf("exact command should close the popup: %v", m.cmdSug)
	}

	// Arguments after a space: popup closed.
	m.textarea.SetValue("/git status --short")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != 0 {
		t.Errorf("args after space should close the popup: %v", m.cmdSug)
	}

	// Unknown prefix: closed.
	m.textarea.SetValue("/zzz")
	m.refreshCmdSuggest()
	if len(m.cmdSug) != 0 {
		t.Errorf("unknown prefix should close the popup: %v", m.cmdSug)
	}
}

func TestCmdSuggestKeys(t *testing.T) {
	m := sugModel()

	// Open suggestions with "/g" → git, github.
	typeKey(m, "/g")
	if len(m.cmdSug) != 2 || m.cmdSug[0] != "git" || m.cmdSug[1] != "github" {
		t.Fatalf("'/g' should suggest [git github], got %v", m.cmdSug)
	}

	// Down moves selection, wraps.
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.cmdSugIdx != 1 {
		t.Errorf("down should select index 1, got %d", m.cmdSugIdx)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.cmdSugIdx != 0 {
		t.Errorf("down should wrap to index 0, got %d", m.cmdSugIdx)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.cmdSugIdx != 1 {
		t.Errorf("up should wrap to last index, got %d", m.cmdSugIdx)
	}

	// Tab accepts the highlighted command into the input.
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got := m.textarea.Value(); got != "/github" {
		t.Errorf("tab should fill '/github', got %q", got)
	}
	if len(m.cmdSug) != 0 {
		t.Errorf("popup should close after accept, got %v", m.cmdSug)
	}

	// Enter executes a highlighted partial in one step.
	m.textarea.SetValue("")
	m.refreshCmdSuggest()
	m.workspace = "/tmp/project"
	typeKey(m, "/pw")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.textarea.Value(); got != "" {
		t.Errorf("enter on a highlighted command should submit and clear input, got %q", got)
	}
	if noticeText(m) != "workspace: /tmp/project" {
		t.Errorf("enter on /pw should execute /pwd, status=%q", noticeText(m))
	}

	// Escape dismisses suggestions without destroying the draft.
	typeKey(m, "/g")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.textarea.Value(); got != "/g" || len(m.cmdSug) != 0 {
		t.Errorf("escape should preserve the draft and close suggestions: value=%q suggestions=%v", got, m.cmdSug)
	}
}

func TestCtrlCTwoStageQuit(t *testing.T) {
	m := sugModel()
	cc := tea.KeyMsg{Type: tea.KeyCtrlC}

	// Busy: interrupt, no quit.
	m.busy = true
	_, cmd := m.handleKey(cc)
	if cmd == nil {
		t.Error("busy ctrl+c should submit an interrupt command")
	} else {
		applyTeaCmd(m, cmd)
	}
	m.busy = false

	// Idle: first press arms, second quits.
	if _, cmd := m.handleKey(cc); cmd != nil {
		t.Error("first ctrl+c when idle should not quit")
	}
	if !m.quitArmed {
		t.Error("first ctrl+c should arm the quit")
	}
	if _, cmd := m.handleKey(cc); cmd == nil {
		t.Error("second ctrl+c should quit")
	}

	// Any other key disarms.
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.quitArmed {
		t.Error("typing should disarm the quit")
	}
	if _, cmd := m.handleKey(cc); cmd != nil {
		t.Error("disarmed ctrl+c must not quit on first press")
	}
}

func TestScrollKeysReachViewport(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(40, 3)
	m.baseVpH = 3
	// Fill with more content than fits and pin to the bottom (app default).
	m.viewport.SetContent("l1\nl2\nl3\nl4\nl5\nl6")
	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset
	if bottom == 0 {
		t.Fatal("content should overflow the 3-line viewport")
	}

	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyPgUp}); m.viewport.YOffset >= bottom {
		t.Error("pgup should scroll the transcript up")
	}
	if m.followOutput {
		t.Error("pgup should stop following new output")
	}
	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyPgDown}); m.viewport.YOffset != bottom {
		t.Error("pgdown should scroll back to the bottom")
	}
	if !m.followOutput {
		t.Error("returning to the bottom should resume following output")
	}
}

func TestMacScrollShortcutsReachViewport(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(40, 3)
	m.viewport.SetContent("l1\nl2\nl3\nl4\nl5\nl6")
	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.viewport.YOffset >= bottom {
		t.Error("⌃U should scroll the transcript up")
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
	if m.viewport.YOffset != bottom {
		t.Error("⌃D should scroll the transcript down")
	}
}

func TestAlternateScrollKeysScrollViewport(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(40, 3)
	m.baseVpH = 3
	m.items = []historyCell{{kind: "system", text: "l1\nl2\nl3\nl4\nl5\nl6"}}
	m.render()
	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.viewport.YOffset >= bottom {
		t.Error("alternate-scroll up key should scroll the transcript up")
	}
	if m.followOutput {
		t.Error("scrolling up should stop following new output")
	}

	before := m.viewport.YOffset
	m.handleProtocolEvent(protocol.EventView{Kind: protocol.EventStatus, SessionID: "test-session", Text: "streaming"})
	if m.viewport.YOffset != before {
		t.Errorf("new events should not yank a scrolled viewport: offset %d -> %d", before, m.viewport.YOffset)
	}

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.viewport.YOffset != bottom || !m.followOutput {
		t.Error("alternate-scroll down key should return to and follow the bottom")
	}
}

func TestArrowKeyEditsMultilineComposerInsteadOfScrolling(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(40, 3)
	m.viewport.SetContent("l1\nl2\nl3\nl4\nl5\nl6")
	m.viewport.GotoBottom()
	before := m.viewport.YOffset
	m.textarea.SetValue("first line\nsecond line")

	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.viewport.YOffset != before {
		t.Error("arrow keys should not scroll while editing multiline input")
	}
}

func TestTextareaStartsSingleLineAndGrows(t *testing.T) {
	m := sugModel()
	if got := m.textarea.Height(); got != 1 {
		t.Fatalf("textarea should start at one line, got %d", got)
	}
	if got := strings.Count(m.textarea.View(), "›"); got != 1 {
		t.Fatalf("single-line textarea should render one prompt, got %d", got)
	}

	m.height = 20
	m.baseVpH = 15
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line one")})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if got := m.textarea.Height(); got != 2 {
		t.Errorf("textarea should grow after a newline, got height %d", got)
	}
}

func TestBusyInputStillRendersComposer(t *testing.T) {
	m := sugModel()
	m.width = 80
	m.busy = true
	out := m.renderInput()
	if !strings.Contains(out, composerPlaceholder) || !strings.Contains(out, "›") {
		t.Fatalf("busy input should keep rendering its placeholder and cursor, got %q", out)
	}
}

func TestDurableSlashCommandAppearsInTranscript(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(80, 10)
	m.textarea.SetValue("/help")

	_, cmd := m.submit()
	if cmd != nil {
		t.Fatal("/help should not return a runtime command")
	}
	if len(m.items) != 2 || m.items[0].kind != "command" || m.items[0].text != "/help" || m.items[1].kind != "system" {
		t.Fatalf("submitted slash command was not recorded: %#v", m.items)
	}
	if len(m.history) != 1 || m.history[0] != "/help" {
		t.Fatalf("slash command should be available in input history: %#v", m.history)
	}
	if out := renderItemWidth(&m.items[0], 80); !strings.Contains(out, "❯ /help") {
		t.Fatalf("rendered command does not show /help: %q", out)
	}
}

func TestModelPermissionAndSandboxCommands(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(80, 10)
	client := m.client.(*recordingClient)
	client.snapshot.Catalog.Providers[0].Models = []string{"test-model", "test-model-2"}
	m.snapshot = client.snapshot
	m.hasSnapshot = true
	m.mode = permissions.ModeDefault
	m.width, m.height = 80, 24
	m.baseVpH = 19

	m.textarea.SetValue("/model")
	_, _ = m.submit()
	if len(m.items) != 0 {
		t.Fatalf("/model should not enter the transcript: %#v", m.items)
	}
	if m.picker == nil || !m.picker.inline || len(m.picker.Options) < 2 {
		t.Fatalf("/model should open an inline model picker: %#v", m.picker)
	}
	if out := m.renderInlineSurface(); !strings.Contains(out, "Select model") || !strings.Contains(out, m.modelName) {
		t.Fatalf("model picker did not render above the composer: %q", out)
	}
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	selectedModel := m.picker.Options[m.picker.Index].Label
	_, cmd := m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	applyTeaCmd(m, cmd)
	if m.picker != nil || m.modelName != "test-model" || noticeText(m) != "model → "+selectedModel {
		t.Fatalf("model selection was not applied: model=%q status=%q picker=%v", m.modelName, noticeText(m), m.picker)
	}
	if got := client.submits[len(client.submits)-1]; got.Type != protocol.CommandSetModel || got.Model.Model != selectedModel {
		t.Fatalf("model selection submitted wrong command: %#v", got)
	}

	m.textarea.SetValue("/mode")
	_, _ = m.submit()
	if len(m.items) != 0 {
		t.Fatalf("/mode should not enter the transcript: %#v", m.items)
	}
	if m.picker == nil || !m.picker.inline || len(m.picker.Options) != len(permissions.ValidModes) {
		t.Fatalf("/mode should replace model status with an inline picker: %#v", m.picker)
	}
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd = m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	applyTeaCmd(m, cmd)
	if m.mode != permissions.ModeDefault || noticeText(m) != "permission mode → edits" {
		t.Fatalf("mode selection was not applied: mode=%s status=%q", m.mode, noticeText(m))
	}
	if got := client.submits[len(client.submits)-1]; got.Type != protocol.CommandSetPermissionPolicy || got.PermissionPolicy.Policy.Mode != string(permissions.ModeAcceptEdits) {
		t.Fatalf("unexpected mode command: %#v", got)
	}

	m.workspace = "/tmp/project"
	m.snapshot.Settings.Sandbox = protocol.SandboxPolicy{
		NetworkAccess:                 true,
		AdditionalDirectories:         []string{"/tmp/project/shared"},
		AdditionalReadOnlyDirectories: []string{"/tmp/reference"},
		DisallowedDirectories:         []string{"/tmp/private"},
		Revision:                      7,
	}
	next, cmd := m.runCommand("/sandbox")
	updated := modelValue(t, next)
	m = &updated
	if m.picker != nil {
		t.Fatalf("/sandbox must be a read-only diagnostic, got picker %#v", m.picker)
	}
	if len(m.reports) == 0 {
		t.Fatal("/sandbox did not render the confirmed policy")
	}
	policyReport := m.reports[len(m.reports)-1].text
	for _, want := range []string{"/tmp/project", "/tmp/project/shared", "/tmp/reference", "/tmp/private", "Network access: authorized", "revision 7"} {
		if !strings.Contains(policyReport, want) {
			t.Errorf("/sandbox diagnostic missing %q: %s", want, policyReport)
		}
	}
	if cmd == nil {
		t.Fatal("/sandbox should request runtime Seatbelt diagnostics")
	}
	applyTeaCmd(m, cmd)
	if got := client.submits[len(client.submits)-1]; got.Type != protocol.CommandQuery || got.Query == nil || got.Query.Kind != protocol.QueryDoctor {
		t.Fatalf("/sandbox must query read-only runtime diagnostics, got %#v", got)
	}

	m.textarea.SetValue("/help")
	_, _ = m.submit()
	if noticeText(m) != "" {
		t.Fatalf("durable command should clear stale transient status, got %q", noticeText(m))
	}
	foundHelp := false
	for _, item := range m.items {
		if item.text == "/help" {
			foundHelp = true
			break
		}
	}
	if !foundHelp {
		t.Fatalf("/help should produce durable transcript output: %#v", m.items)
	}
}

func TestSlashCommandOutputLifetime(t *testing.T) {
	tests := []struct {
		command string
		durable bool
	}{
		{"/model", false},
		{"/mode default", false},
		{"/sandbox", true},
		{"/config", true},
		{"/config set model test", false},
		{"/permissions", true},
		{"/permissions allow Bash:git status", false},
		{"/memory", true},
		{"/memory clear", false},
		{"/skills", true},
		{"/unknown", true},
	}
	for _, tt := range tests {
		if got := slashCommandHasTranscriptOutput(tt.command); got != tt.durable {
			t.Errorf("slashCommandHasTranscriptOutput(%q) = %v, want %v", tt.command, got, tt.durable)
		}
	}
}

func TestSandboxCatalogIsPersistentDiagnosticWithRevokeUsage(t *testing.T) {
	entry, ok := commandCatalog.Lookup("sandbox")
	if !ok {
		t.Fatal("sandbox command is missing from the TUI catalog")
	}
	if entry.Feedback != commands.FeedbackReport || !entry.Transcript || !entry.Mutation {
		t.Fatalf("sandbox should report persistent diagnostics and support a mutating revoke action, got %#v", entry)
	}
	if !strings.Contains(entry.Help, "[revoke all]") || !strings.Contains(entry.Help, "revoke all session access") {
		t.Fatalf("sandbox help should document /sandbox [revoke all], got %q", entry.Help)
	}
	if !commandCatalog.ShouldTranscript("sandbox", nil) || !slashCommandHasTranscriptOutput("/sandbox") {
		t.Fatal("sandbox diagnostic should keep its output in the transcript")
	}
}

func TestSubmitPreservesPromptWhitespace(t *testing.T) {
	m := sugModel()
	m.textarea.SetValue("  indented code\n")
	_, cmd := m.submit()
	applyTeaCmd(m, cmd)
	client := m.client.(*recordingClient)
	if len(client.submits) != 1 || client.submits[0].Input.Text != "  indented code\n" {
		t.Fatalf("normal prompt whitespace was changed: %#v", client.submits)
	}
}

func TestUserMessageUsesCompactPromptStyle(t *testing.T) {
	oneLine := renderItemWidth(&historyCell{kind: "user", text: "你好"}, 80)
	if !strings.Contains(oneLine, "› ") {
		t.Fatalf("user message should render prompt prefix: %q", oneLine)
	}
	if !strings.Contains(oneLine, "你好") {
		t.Fatalf("user message should contain text: %q", oneLine)
	}

	multiline := renderItemWidth(&historyCell{kind: "user", text: "first\nsecond"}, 80)
	if !strings.Contains(multiline, "› first") || !strings.Contains(multiline, "\n  second") {
		t.Fatalf("continuation lines should align below the message text: %q", multiline)
	}
}

func TestAssistantMessageDoesNotSpendRowOnLabel(t *testing.T) {
	out := renderItemWidth(&historyCell{kind: "assistant", text: "answer"}, 80)
	if strings.Contains(out, "Assistant") || strings.Contains(out, "\n") {
		t.Fatalf("assistant output should render one row without a role header: %q", out)
	}
	if !strings.Contains(out, "answer") {
		t.Fatalf("assistant output should contain answer: %q", out)
	}
}

func TestClearDoesNotLeaveCommandInClearedTranscript(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(80, 10)
	m.items = append(m.items, historyCell{kind: "system", text: "old output"})
	m.snapshot.History = []protocol.MessageView{{Role: "user", Content: "old"}}
	m.textarea.SetValue("/clear")

	_, cmd := m.submit()
	applyTeaCmd(m, cmd)
	if len(m.items) != 1 || m.items[0].text != "old output" {
		t.Fatalf("/clear must wait for confirmed snapshot, got %#v", m.items)
	}
	snapshot := m.snapshot
	snapshot.History = nil
	m.applySnapshot(snapshot)
	if len(m.items) != 0 {
		t.Fatalf("confirmed clear should leave an empty transcript, got %#v", m.items)
	}
}

func TestAltEnterInsertsNewline(t *testing.T) {
	m := sugModel()
	keys := m.textarea.KeyMap.InsertNewline.Keys()
	foundAlt, foundCtrlJ := false, false
	for _, k := range keys {
		switch k {
		case "alt+enter":
			foundAlt = true
		case "ctrl+j":
			foundCtrlJ = true
		}
	}
	if !foundAlt || !foundCtrlJ {
		t.Errorf("InsertNewline must bind alt+enter and ctrl+j, got %v", keys)
	}

	// Typing a line then alt+enter keeps the text and opens a new line.
	typeKey(m, "line one")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if !strings.HasPrefix(m.textarea.Value(), "line one") {
		t.Errorf("alt+enter lost the text: %q", m.textarea.Value())
	}
	if !strings.Contains(m.textarea.Value(), "\n") {
		t.Errorf("alt+enter should insert a newline, got %q", m.textarea.Value())
	}
	if m.textarea.Height() != 2 {
		t.Errorf("alt+enter should grow the textarea to two rows, got %d", m.textarea.Height())
	}
}

func TestQuitCommandReturnsQuitMsg(t *testing.T) {
	m := sugModel()
	for _, input := range []string{"/quit", "/exit"} {
		_, cmd := m.runCommand(input)
		if cmd == nil {
			t.Errorf("%s should return a quit command", input)
			continue
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Errorf("%s should produce a tea.QuitMsg, got %T", input, msg)
		}
	}
}

func TestCmdSuggestScrollsThroughAllMatches(t *testing.T) {
	m := sugModel()
	m.textarea.SetValue("/")
	m.refreshCmdSuggest()
	if len(m.cmdSug) <= maxCmdSuggestions {
		t.Skip("not enough commands to exercise scrolling")
	}
	for range maxCmdSuggestions {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.cmdSugIdx != maxCmdSuggestions {
		t.Fatalf("selection should move beyond the first window, got %d", m.cmdSugIdx)
	}
	out := m.renderCmdSuggest()
	if !strings.Contains(out, "/"+m.cmdSug[m.cmdSugIdx]) || !strings.Contains(out, "of ") {
		t.Errorf("scrolled popup should show the selected command and position: %q", out)
	}

	m.handleKey(tea.KeyMsg{Type: tea.KeyEnd})
	if m.cmdSugIdx != len(m.cmdSug)-1 {
		t.Errorf("end should select the final command, got %d", m.cmdSugIdx)
	}
	want := "/" + m.cmdSug[m.cmdSugIdx]
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got := m.textarea.Value(); got != want {
		t.Errorf("tab should accept a command outside the first window: got %q, want %q", got, want)
	}

	m.textarea.SetValue("/")
	m.refreshCmdSuggest()
	for range len(m.cmdSug) {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.cmdSugIdx != 0 {
		t.Errorf("down should wrap across all %d entries, got index %d", len(m.cmdSug), m.cmdSugIdx)
	}
}

func TestPickerSupportsScrollingAndDirectNumbers(t *testing.T) {
	m := sugModel()
	m.width, m.height = 80, 18
	lines := make([]string, 25)
	for i := range lines {
		lines[i] = "item " + strconv.Itoa(i+1)
	}
	options := make([]SelectorOption, len(lines))
	for i, line := range lines {
		options[i] = SelectorOption{ID: strconv.Itoa(i), Label: line}
	}
	client := m.client.(*recordingClient)
	m.startSelectorAt("choose item", options, 0, false, selectorAction{Kind: selectorModel})
	for range 12 {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if m.picker.Index != 12 {
		t.Fatalf("picker should navigate beyond the first page, got %d", m.picker.Index)
	}
	if out := m.renderInlineSurface(); !strings.Contains(out, "item 13") || !strings.Contains(out, "13/25") {
		t.Fatalf("picker should render its scrolled selection: %q", out)
	} else if got := lipgloss.Height(out); got > m.height {
		t.Fatalf("picker height %d exceeds terminal height %d", got, m.height)
	}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.picker != nil {
		t.Fatalf("enter should choose highlighted item 13, picker=%v", m.picker)
	}
	if cmd == nil {
		t.Fatal("typed picker selection should submit a protocol command")
	}
	applyTeaCmd(m, cmd)
	if got := client.submits[len(client.submits)-1]; got.Type != protocol.CommandSetModel || got.Model == nil || got.Model.Model != "12" {
		t.Fatalf("highlighted item 13 should submit its stable option id, got %#v", got)
	}

	m.startSelectorAt("choose item", options, 0, false, selectorAction{Kind: selectorModel})
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("20")})
	_, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("numbered picker selection should submit a protocol command")
	}
	applyTeaCmd(m, cmd)
	if got := client.submits[len(client.submits)-1]; got.Type != protocol.CommandSetModel || got.Model == nil || got.Model.Model != "19" {
		t.Fatalf("typing 20 should submit stable option id 19, got %#v", got)
	}
}

func TestInlinePickerRendersAboveComposerAndReservesSpace(t *testing.T) {
	m := sugModel()
	m.width, m.height = 80, 20
	m.baseVpH = 15
	m.viewport = viewport.New(80, m.baseVpH)
	options := []SelectorOption{{ID: "one", Label: "one"}, {ID: "two", Label: "two"}, {ID: "three", Label: "three"}}
	client := m.client.(*recordingClient)
	m.startSelectorAt("Select value", options, 1, true, selectorAction{Kind: selectorModel})
	if m.picker == nil || !m.picker.inline || m.picker.Index != 1 {
		t.Fatalf("inline picker did not preserve current selection: %#v", m.picker)
	}
	if m.viewport.Height != m.baseVpH {
		t.Fatalf("inline picker must not reserve transcript rows: height=%d base=%d", m.viewport.Height, m.baseVpH)
	}
	out := m.renderInlineSurface()
	if !strings.Contains(out, "Select value") || !strings.Contains(out, "two") {
		t.Fatalf("inline picker did not render its choices: %q", out)
	}
	_, cmd := m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.picker != nil || m.viewport.Height != m.baseVpH {
		t.Fatalf("inline selection did not close and restore layout: picker=%v height=%d", m.picker, m.viewport.Height)
	}
	if cmd == nil {
		t.Fatal("inline picker selection should submit a protocol command")
	}
	applyTeaCmd(m, cmd)
	if got := client.submits[len(client.submits)-1]; got.Type != protocol.CommandSetModel || got.Model == nil || got.Model.Model != "two" {
		t.Fatalf("inline picker should submit the selected stable id, got %#v", got)
	}
}

func TestLongApprovalCanBeScrolledOnSmallTerminal(t *testing.T) {
	m := sugModel()
	m.width, m.height = 44, 15
	m.approval = &approvalPrompt{
		ID:      "approval-1",
		Tool:    "Bash",
		Command: strings.Repeat("long-command-argument ", 20),
		Reason:  "review before allowing",
	}
	if maxOffset := max(0, len(m.approvalDetailLines())-m.approvalVisibleRows()); maxOffset == 0 {
		t.Fatal("long approval should overflow the small modal")
	}
	m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyEnd})
	if m.approvalState.Offset != max(0, len(m.approvalDetailLines())-m.approvalVisibleRows()) {
		t.Fatalf("end should reach the final approval page: offset=%d max=%d", m.approvalState.Offset, max(0, len(m.approvalDetailLines())-m.approvalVisibleRows()))
	}
	if out := m.renderApprovalInline(); !strings.Contains(out, "PgUp/PgDn") {
		t.Fatalf("scrollable approval should show its position: %q", out)
	} else if got := lipgloss.Height(out); got > m.height {
		t.Fatalf("approval height %d exceeds terminal height %d", got, m.height)
	}
}

func TestApprovalArrowSelectsChoice(t *testing.T) {
	m := sugModel()
	m.width, m.height = 44, 15
	m.approval = &approvalPrompt{ID: "a", Tool: "Bash", Command: "echo ok", Reason: "r"}

	if _, _ = m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyDown}); m.approvalCursor != 1 {
		t.Fatalf("down should select 'Always allow', cursor=%d", m.approvalCursor)
	}
	out := m.renderApprovalInline()
	if !strings.Contains(out, "❯ Always allow") {
		t.Fatalf("render should highlight the selected choice:\n%s", out)
	}
}

func TestApprovalArrowClampsAtBounds(t *testing.T) {
	m := sugModel()
	m.approval = &approvalPrompt{ID: "a", Tool: "Bash", Command: "echo ok"}

	m.approvalCursor = 2
	m.approvalSelection, m.approvalSelected = m.approvalKey(), true
	if _, _ = m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyDown}); m.approvalCursor != 2 {
		t.Fatalf("down beyond last choice should clamp, cursor=%d", m.approvalCursor)
	}
	m.approvalCursor = 0
	if _, _ = m.handleApprovalKey(tea.KeyMsg{Type: tea.KeyUp}); m.approvalCursor != 0 {
		t.Fatalf("up above first choice should clamp, cursor=%d", m.approvalCursor)
	}
}

func TestWorkspaceCommandsUseSessionProtocol(t *testing.T) {
	m := sugModel()

	if cmd := m.runDiff(nil); cmd == nil {
		t.Error("runDiff should return a tea.Cmd")
	} else if msg, ok := cmd().(commandReceiptMsg); !ok {
		t.Errorf("runDiff cmd should yield commandReceiptMsg, got %T", msg)
	}

	if cmd := m.runGit([]string{"status", "--short"}); cmd == nil {
		t.Error("runGit should return a tea.Cmd")
	} else if msg, ok := cmd().(commandReceiptMsg); !ok {
		t.Errorf("runGit cmd should yield commandReceiptMsg, got %T", msg)
	}

	if _, cmd := m.runCommand("/diff"); cmd == nil {
		t.Error("/diff discarded its asynchronous command")
	}
}

func TestTranscriptWrapsLongLines(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(16, 10)
	m.viewport.Style = lipgloss.NewStyle().Padding(0, 1)
	m.items = []historyCell{{kind: "error", text: strings.Repeat("x", 40)}}
	m.render()
	if got := m.viewport.TotalLineCount(); got < 3 {
		t.Fatalf("long transcript line should wrap instead of being clipped, got %d line(s)", got)
	}
}

func TestSmallTerminalStillLaysOut(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(80, 20)
	m.width, m.height = 15, 7
	m.layout()
	if m.viewport.Width != 15 || m.viewport.Height < 1 || m.textarea.Width() < 1 {
		t.Fatalf("small terminal retained invalid/default dimensions: viewport=%dx%d textarea=%d",
			m.viewport.Width, m.viewport.Height, m.textarea.Width())
	}
}

func TestPopupReservesViewportSpace(t *testing.T) {
	m := sugModel()
	m.viewport = viewport.New(40, 20)
	m.baseVpH = 20

	m.textarea.SetValue("/")
	m.refreshCmdSuggest()
	if len(m.cmdSug) == 0 {
		t.Fatal("popup should open for '/'")
	}
	visible := min(len(m.cmdSug), maxCmdSuggestions)
	want := 20 - visible - 3
	if len(m.cmdSug) > visible {
		want-- // position hint line
	}
	if want < 3 {
		want = 3
	}
	if m.viewport.Height != want {
		t.Errorf("viewport height = %d, want %d (popup %d entries + hint)", m.viewport.Height, want, len(m.cmdSug))
	}

	m.closeCmdSuggest()
	if m.viewport.Height != 20 {
		t.Errorf("closing the popup should restore height 20, got %d", m.viewport.Height)
	}
}

func TestInlineArrowsWithoutHistoryKeepFrameStable(t *testing.T) {
	for _, draft := range []string{"", "unfinished prompt"} {
		for _, flushed := range []bool{false, true} {
			m := NewWithClient(sugModel().client, "/workspace", false)
			m.width, m.height = 100, 30
			m.textarea.SetValue(draft)
			m.layout()
			if flushed {
				planTestHistory(&m)
			}
			before := m.View()
			offset := m.viewport.YOffset
			for _, key := range []tea.KeyType{tea.KeyUp, tea.KeyDown, tea.KeyUp, tea.KeyDown} {
				_, cmd := m.handleKey(tea.KeyMsg{Type: key})
				if cmd != nil {
					t.Fatal("empty history unexpectedly produced a command")
				}
				if !m.followOutput || m.viewport.YOffset != offset || m.View() != before || m.textarea.Value() != draft {
					t.Fatalf("arrow changed inline frame: key=%v draft=%q flushed=%v follow=%v offset=%d", key, draft, flushed, m.followOutput, m.viewport.YOffset)
				}
			}
		}
	}
}

func TestInlineArrowsStillRecallHistory(t *testing.T) {
	m := inlineTestModel()
	m.history = []string{"previous prompt"}
	m.historyIdx = len(m.history)
	m.textarea.SetValue("")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.textarea.Value() != "previous prompt" || !m.followOutput {
		t.Fatalf("up did not recall history: %q", m.textarea.Value())
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.textarea.Value() != "" || !m.followOutput {
		t.Fatalf("down did not return to empty prompt: %q", m.textarea.Value())
	}
}
