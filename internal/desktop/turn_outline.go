package desktop

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"
)

type turnMarker struct {
	row                 int
	id, question, reply string
}

// Use the same rows as the virtual list, including its paged-in leading
// prompt. Expanding activity or prepending history can change every index.
func turnMarkers(rows []transcriptRow) []turnMarker {
	var markers []turnMarker
	for i, row := range rows {
		item := row.item
		if item == nil || row.detail {
			continue
		}
		switch item.Kind {
		case "user":
			markers = append(markers, turnMarker{row: i, id: item.ID, question: item.Text})
		case "assistant":
			if n := len(markers); n > 0 && item.Text != "" && sameTurn(rows[markers[n-1].row].item.TurnID, item.TurnID) {
				markers[n-1].reply = item.Text
			}
		}
	}
	return markers
}

func turnOutline(c *ui.Context, conv *conversation, rows []transcriptRow) float32 {
	markers := turnMarkers(rows)
	if len(markers) == 0 {
		return 0
	}
	first, _ := conv.list.Visible()
	active := 0
	for i, m := range markers {
		if m.row <= first {
			active = i
		}
	}
	if conv.list.AtEnd() {
		active = len(markers) - 1
	}
	const width, step = 38, 14
	t := c.Theme()
	ui.Scroll(c.Key("turn-outline")).Role(ui.RoleGroup).Label("对话节点").Width(width).Shrink(0).
		Height(float32(len(markers)) * step).MaxHeightPercent(90).AlignSelf(ui.Center).Scrollbars(false).Children(func() {
		for i, m := range markers {
			question := strings.Join(strings.Fields(shorten(m.question, 96)), " ")
			b := ui.ButtonBase(c.Key(m.id)).Label(fmt.Sprintf("跳转到提问 %d", i+1)).Description(question).
				Height(step).Shrink(0).Padding(0, 0, 0, 9).Justify(ui.Start).AlignItems(ui.Center)
			line, color := float32(7), t.Border
			if i == active || b.Hovered() || b.FocusVisible() {
				line, color = 20, t.Text
			}
			if i == active {
				b.Current(ui.CurrentStep)
				if conv.outlineActive != m.id {
					b.ScrollIntoView()
					conv.outlineActive = m.id
				}
			}
			b.Children(func() { ui.Box(c).Size(line, 2).Radius(1).Background(color) }).OnClick(func() {
				conv.list.ScrollTo(m.row, ui.Start)
			})
			ui.TooltipBase(c, b, func(tip ui.Element) {
				tip.AttachTo(b, ui.AnchorEnd, ui.AnchorStart).MarginStart(6).Width(300).Padding(10, 12).Gap(5).
					Radius(10).Background(t.Background).Border(1, t.Border).Shadow(0, 4, 14, 0, ui.RGBA(0, 0, 0, .12)).Children(func() {
					ui.Text(c, question).FontSize(13).FontWeight(600).MaxLines(2)
					if m.reply != "" {
						ui.Text(c, strings.Join(strings.Fields(shorten(m.reply, 180)), " ")).FontSize(12).MaxLines(3).TextColor(t.TextMuted)
					}
				})
			})
		}
	})
	return width
}
