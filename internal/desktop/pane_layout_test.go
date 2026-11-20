package desktop

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
)

func TestChatDividerDragAndRestore(t *testing.T) {
	a, conv, _ := testApp(t)
	const window = 1320
	conv.inspector.open = true
	tt := ui.NewTester(a.view, window, 800)
	drag := func(x, dx float32) {
		t.Helper()
		tt.Move(x, 240)
		if tt.Cursor() != ui.CursorResizeEW {
			t.Fatalf("no resize cursor on divider at %v", x)
		}
		tt.Press(x, 240)
		tt.Move(x+dx, 240)
		tt.Release(x+dx, 240)
	}
	right := func() ui.Rect {
		t.Helper()
		box, ok := tt.Find("右栏")
		if !ok {
			t.Fatal("inspector disappeared")
		}
		return box
	}
	initial := right()
	drag(navigationRailWidth+projectSidebarWidth, 42)
	if a.sidebarWidth != projectSidebarWidth+42 || right() != initial {
		t.Fatalf("dragging sidebar changed the wrong pane: sidebar=%v inspector=%+v", a.sidebarWidth, right())
	}
	drag(initial.X-1, -65)
	wider := right()
	if math.Abs(float64(wider.W-initial.W-65)) > 1 {
		t.Fatalf("right drag: before=%+v after=%+v", initial, wider)
	}
	tt.SetSize(780, 580)
	if _, ok := tt.Find("搜索项目或会话"); ok {
		t.Fatal("sidebar overlapped the compact inspector")
	}
	send, ok := tt.Find("发送消息")
	if !ok || send.X+send.W > right().X {
		t.Fatalf("inspector overlaps the composer at minimum window size: %+v %+v", send, right())
	}
	tt.SetSize(window, 800)
	conv.inspector.open = false
	tt.Frame()
	conv.inspector.open = true
	tt.Frame()
	if got := right(); got != wider {
		t.Fatalf("inspector width lost after resizing and reopening: %+v want %+v", got, wider)
	}
}

func TestSecondaryPageDividers(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*app)
	}{
		{"settings", func(a *app) { a.settingsPage = &settingsPage{} }},
		{"project", func(a *app) { a.projectPage = &projectPage{path: "/work/ccdp"} }},
		{"customization", func(a *app) { a.pluginsPage = &customizationPage{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := testApp(t)
			tc.open(a)
			tt := ui.NewTester(a.view, 1180, 800)
			split, ok := tt.AccessByRole(ui.RoleSplitter, "")
			if !ok {
				t.Fatal("missing sidebar divider")
			}
			x, y := split.Bounds.X, split.Bounds.Y+100
			tt.Press(x, y)
			tt.Move(x+60, y)
			tt.Release(x+60, y)
			after, _ := tt.AccessByRole(ui.RoleSplitter, "")
			if after.Bounds.X != x+60 {
				t.Fatalf("divider moved from %v to %v", x, after.Bounds.X)
			}
		})
	}
}

func TestHiddenSidebarTurnOutline(t *testing.T) {
	a, conv, _ := testApp(t)
	conv.list.FollowEnd = true
	conv.client = &fakeRuntime{}
	for i := 1; i <= 20; i++ {
		id := fmt.Sprint(i)
		conv.view.Transcript = append(conv.view.Transcript,
			protocol.TranscriptItem{ID: "q" + id, Kind: "user", TurnID: protocol.TurnID(id), Text: "问题 " + id},
			protocol.TranscriptItem{ID: "a" + id, Kind: "assistant", TurnID: protocol.TurnID(id), Text: strings.Repeat("回答 "+id+"。", 28)})
	}
	tt := ui.NewTester(a.view, 1180, 700)
	if _, ok := tt.Find("对话节点"); ok {
		t.Fatal("outline visible beside the project sidebar")
	}
	if err := tt.Click("收起侧栏"); err != nil {
		t.Fatal(err)
	}
	button, ok := tt.Find("跳转到提问 4")
	if !ok || button.H < 10 || button.X < navigationRailWidth {
		t.Fatalf("missing turn hit target: %+v", button)
	}
	tt.Move(button.X+4, button.Y+button.H/2)
	tt.Advance(time.Second)
	if _, ok := tt.AccessByRole(ui.RoleTooltip, ""); !ok {
		t.Fatal("turn preview did not open on hover")
	}
	captureSidebar(t, tt, "turn-outline.png")
	before, _ := conv.list.Visible()
	if err := tt.Click("跳转到提问 4"); err != nil {
		t.Fatal(err)
	}
	first, last := conv.list.Visible()
	if first >= before || first > 6 || last < 6 || a.current != "s" {
		t.Fatalf("jump to fourth question: before=%d after=%d..%d session=%q", before, first, last, a.current)
	}
	if err := tt.Click("显示侧栏"); err != nil {
		t.Fatal(err)
	}
	if _, ok := tt.Find("跳转到提问 4"); ok {
		t.Fatal("turn markers still visible")
	}
	first, last = conv.list.Visible()
	if first > 6 || last < 6 {
		t.Fatalf("restoring the sidebar lost the selected question: %d..%d", first, last)
	}
}
