package desktop

import (
	"testing"
	"time"

	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
)

func TestRunningCommandIsVisibleAndPulsesUntilExit(t *testing.T) {
	a, conv, _ := testApp(t)
	conv.client = &fakeRuntime{}
	conv.view.Transcript = []protocol.TranscriptItem{
		{ID: "cmd", Kind: "tool", Tool: "Bash", Status: "success", Process: &protocol.ProcessState{
			ProcessInfo: protocol.ProcessInfo{Command: "go test ./..."}, ProcessOutput: protocol.ProcessOutput{Status: "running"},
		}},
		{ID: "read", Kind: "tool", Tool: "Read", Status: "success"},
	}
	tt := ui.NewTester(a.view, 1000, 650)
	const label = "正在运行 go test ./..."
	b, ok := tt.Find(label)
	if !ok {
		t.Fatal("later finished tool hid the live command")
	}
	darkest := func() uint8 {
		result := uint8(255)
		img := tt.Image()
		for y := int(b.Y); y < int(b.Y+b.H); y++ {
			for x := int(b.X); x < int(b.X+b.W); x++ {
				result = min(result, img.RGBAAt(x, y).R)
			}
		}
		return result
	}
	idle := darkest()
	tt.Advance(800 * time.Millisecond)
	if darkest() >= idle {
		t.Fatal("running command did not pulse")
	}
	captureSidebar(t, tt, "running-command.png")
	tt.SetPreferences(ui.Preferences{ReduceMotion: true})
	still := darkest()
	tt.Advance(800 * time.Millisecond)
	if darkest() != still || !tt.HasText(label) {
		t.Fatal("reduced motion lost the stable command label")
	}
	exit := 0
	conv.view.Transcript[0].Process.ProcessOutput = protocol.ProcessOutput{Status: "exited", ExitCode: &exit}
	conv.view.Transcript = append(conv.view.Transcript, protocol.TranscriptItem{ID: "done", Kind: "turn_summary", Status: "succeeded", DurationMs: 3000})
	tt.Frame()
	if tt.HasText(label) || !tt.HasText("用时 3 秒") {
		t.Fatal("completed command still displayed as running")
	}
	if err := tt.Click("用时 3 秒"); err != nil || !tt.HasText("go test ./...") {
		t.Fatal("completed command not available in history", err)
	}
}

func TestActivityRowHoverAndDetails(t *testing.T) {
	a, conv, _ := testApp(t)
	conv.client = &fakeRuntime{}
	conv.view.Transcript = []protocol.TranscriptItem{
		{ID: "question", Kind: "user", Text: "检查这次改动"},
		{ID: "read", Kind: "tool", Tool: "Read", Status: "success", Args: []byte(`{"file_path":"internal/desktop/view.go"}`), Text: "package desktop"},
		{ID: "search", Kind: "tool", Tool: "Rg", Status: "success", Args: []byte(`{"pattern":"modelPicker","path":"internal"}`)},
		{ID: "cmd", Kind: "tool", Tool: "Bash", Status: "success", Args: []byte(`{"command":"git status --short"}`), Text: " M internal/desktop/view.go"},
		{ID: "answer", Kind: "assistant", Text: "检查完成。"},
		{ID: "summary", Kind: "turn_summary", DurationMs: 22000, Status: "succeeded"},
	}
	tt := ui.NewTester(a.view, 1180, 800)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	click("用时 22 秒")
	row, ok := tt.AccessByRole(ui.RoleButton, "git status --short")
	if !ok || row.Expanded || tt.HasText(" M internal/desktop/view.go") ||
		!tt.HasText("已读取 view.go") || !tt.HasText("已在 internal 中搜索「modelPicker」") {
		t.Fatalf("collapsed row: %+v", row)
	}
	// Sample the icon and text, away from the right-hand hover chevron.
	b := row.Bounds
	darkest := func() uint8 {
		minimum := uint8(255)
		img := tt.Image()
		for y := int(b.Y); y < int(b.Y+b.H); y++ {
			for x := int(b.X); x < int(b.X)+90; x++ {
				minimum = min(minimum, img.RGBAAt(x, y).R)
			}
		}
		return minimum
	}
	idle := darkest()
	tt.Move(b.X+15, b.Y+b.H/2)
	if hover := darkest(); hover >= idle {
		t.Fatalf("hover did not darken row: %d -> %d", idle, hover)
	}
	click("git status --short")
	info, _ := tt.AccessByRole(ui.RoleButton, "git status --short")
	for _, label := range []string{"终端", "$ git status --short", " M internal/desktop/view.go", "成功", "检查完成。"} {
		if !tt.HasText(label) {
			t.Errorf("missing %q in expanded details", label)
		}
	}
	if !info.Expanded || tt.HasText("package desktop") {
		t.Fatal("click should only expand the selected activity")
	}
	tt.SetScale(2)
	captureSidebar(t, tt, "activity-details.png")
	click("git status --short")
	click("已读取 view.go")
	if tt.HasText("$ git status --short") || !tt.HasText("package desktop") {
		t.Fatal("could not switch between independent details")
	}
}

func TestActivityFailureLabels(t *testing.T) {
	for _, tc := range []struct{ status, label string }{
		{"denied", "编辑 main.go · 已拒绝"}, {"error", "编辑 main.go · 失败"}, {"resource_limit", "编辑 main.go · 已达上限"},
	} {
		item := protocol.TranscriptItem{Kind: "tool", Tool: "Edit", Status: tc.status, Args: []byte(`{"file_path":"src/main.go"}`)}
		if _, got := activitySummary(item); got != tc.label || workItem(item) {
			t.Fatalf("%s: %q", tc.status, got)
		}
	}
	// A tool may have successfully launched a command that later failed.
	code := 1
	item := protocol.TranscriptItem{Kind: "tool", Tool: "Bash", Status: "success", Process: &protocol.ProcessState{
		ProcessInfo:   protocol.ProcessInfo{Command: "go test ./..."},
		ProcessOutput: protocol.ProcessOutput{Status: "exited", ExitCode: &code, Output: "FAIL"},
	}}
	if _, got := activitySummary(item); got != "go test ./... · 退出码 1" || workItem(item) {
		t.Fatal(got)
	}
	if heading, input, out := activityContents(item, ""); heading != "终端" || input != "$ go test ./..." || out != "FAIL" {
		t.Fatalf("%q %q %q", heading, input, out)
	}
}
