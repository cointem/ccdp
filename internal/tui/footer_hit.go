package tui

import (
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
)

type footerHitCell struct {
	x, y, width int
	kind        string
}

// Hit testing uses the same rendered footer as the frame, including wrapping,
// indentation and the extra hint row on narrow terminals. Whitespace is ignored
// only for matching labels across wraps, never made clickable itself.
func (m *Model) footerHitCells() []footerHitCell {
	_, labels := m.persistentStatusContent()
	rows := presentationRows(m.composeChrome().footer, m.width)
	var flat strings.Builder
	var cells []footerHitCell
	for i, row := range rows {
		x := 0
		g := uniseg.NewGraphemes(stripANSI(row))
		for g.Next() {
			text, width := g.Str(), g.Width()
			if strings.TrimSpace(text) != "" {
				flat.WriteString(text)
				for range []byte(text) {
					cells = append(cells, footerHitCell{x: x, y: m.height - len(rows) + i, width: width})
				}
			}
			x += width
		}
	}
	var hits []footerHitCell
	for _, label := range []struct{ kind, text string }{{"model", labels.model}, {"permission", labels.permission}, {"context", labels.context}, {"cache", labels.cache}} {
		needle := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, label.text)
		if needle == "" {
			continue
		}
		start := strings.Index(flat.String(), needle)
		if start < 0 {
			continue
		}
		for _, cell := range cells[start : start+len(needle)] {
			cell.kind = label.kind
			hits = append(hits, cell)
		}
	}
	return hits
}
