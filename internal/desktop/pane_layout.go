package desktop

import "github.com/egoist/mygo/ui"

type paneWidth struct {
	preference                        *float32
	width, fallback, minimum, maximum float32
}

func measurePane(preference *float32, fallback, minimum, maximum float32) paneWidth {
	maximum = max(0, maximum)
	minimum = min(minimum, maximum)
	width := *preference
	if width == 0 {
		width = fallback
	}
	return paneWidth{preference: preference, width: min(max(width, minimum), maximum), fallback: fallback, minimum: minimum, maximum: maximum}
}

// MyGo measures a split from the leading edge. Store the inspector's width
// from the trailing edge so changing the window or sidebar does not move it.
func (p paneWidth) split(c *ui.Context, total float32, trailing bool, first, second func()) {
	firstWidth, reset := p.width, p.fallback
	if trailing {
		firstWidth, reset = total-1-firstWidth, total-1-reset
	}
	if ui.Split(c, &firstWidth, first, second).Fill().SplitReset(reset).Changed() {
		want := firstWidth
		if trailing {
			want = total - 1 - firstWidth
		}
		*p.preference = min(max(want, p.minimum), p.maximum)
		if firstWidth == reset {
			*p.preference = 0
		}
	}
}

type chatLayout struct {
	left, right paneWidth
	main, total float32
	compact     bool
}

func (a *app) chatLayout(width float32, conv *conversation) chatLayout {
	const minMain, minRight = 420, 248
	room := width - navigationRailWidth
	hasRight := conv != nil && conv.inspector.open
	l := chatLayout{total: room, main: room, compact: hasRight && width < 1080}
	if a.sidebar && !l.compact {
		maxLeft := room - minMain - 1
		if hasRight {
			maxLeft -= minRight + 1
		}
		l.left = measurePane(&a.sidebarWidth, projectSidebarWidth, 168, maxLeft)
		l.main -= l.left.width + 1
	}
	if hasRight {
		l.right = measurePane(&a.detailWidth, min(560, max(288, width*.34)), minRight, l.main-minMain-1)
		l.main -= l.right.width + 1
		if l.left.width > 0 {
			l.left.maximum = room - minMain - l.right.width - 2
		}
	}
	return l
}

func (a *app) settingsSidebar(room float32) paneWidth {
	fallback := float32(222)
	if room < 900 {
		fallback = 182
	}
	return measurePane(&a.sidebarWidth, fallback, 164, room-420-1)
}
