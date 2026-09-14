package tui

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestManualGopherPreview(t *testing.T) {
	path := os.Getenv("CCDP_GOPHER_PREVIEW")
	if path == "" {
		t.Skip("manual render capture")
	}
	profile, dark := lipgloss.ColorProfile(), lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile); lipgloss.SetHasDarkBackground(dark) })
	views := map[string]string{}
	for _, theme := range []string{"dark", "light"} {
		lipgloss.SetHasDarkBackground(theme == "dark")
		m := NewWithClient(nil, "/workspace/ccdp", false)
		t.Cleanup(m.watchCancel)
		m.width, m.height = 88, 32
		m.modelName, m.mode, m.sessionID = "deepseek-chat", "default", "local-preview"
		m.textarea.Blur()
		m.items = []logItem{welcomeItem(m.workspace)}
		m.layout()
		views[theme+"-welcome"] = m.View()
		tool, meta := renderToolText("Read", map[string]any{"file_path": "internal/tui/gopher.go"}, "success", "Gopher is ready to help.")
		m.items = []logItem{
			{kind: "user", text: "Use the original standing Gopher in our terminal."},
			{kind: "assistant", text: "The original silhouette, sampled directly into terminal cells."},
			{kind: "tool", text: tool, toolMeta: meta, status: "success"},
		}
		m.busy, m.turnDone = true, false
		m.status, m.turnStarted = "Polishing the pixels…", time.Now().Add(-12*time.Second)
		m.layout()
		views[theme+"-working"] = m.View()
	}
	data, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := strings.ReplaceAll(string(source), "14px/1.6 Menlo", "14px/1.2 Menlo")
	page = strings.ReplaceAll(page, "ccdp / Actual TUI renderer", "ccdp / Gopher companion")
	page = strings.ReplaceAll(page, "fetch('views.json').then(r=>r.json()).then(data=>{views=data;show('dark')});", "/*CCDP_PREVIEW_START*//*CCDP_PREVIEW_END*/")
	pattern := regexp.MustCompile(`(?s)/\*CCDP_PREVIEW_START\*/.*?/\*CCDP_PREVIEW_END\*/`)
	page = pattern.ReplaceAllStringFunc(page, func(string) string {
		return "/*CCDP_PREVIEW_START*/views=" + string(data) + ";show('dark');/*CCDP_PREVIEW_END*/"
	})
	if err := os.WriteFile(path, []byte(page), 0600); err != nil {
		t.Fatal(err)
	}

	// Inspect the same square pixels that the terminal half-blocks display.
	const scale, padding = 12, 4
	pixels := strings.Split(gopherPixels, "\n")
	canvas := image.NewRGBA(image.Rect(0, 0, (gopherWidth+padding*2)*scale, (len(pixels)+padding*2)*scale))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{20, 36, 44, 255}), image.Point{}, draw.Src)
	for y, row := range pixels {
		for x, key := range []byte(row) {
			if key == '.' {
				continue
			}
			var r, g, b uint8
			fmt.Sscanf(string(gopherPalette[key]), "#%02x%02x%02x", &r, &g, &b)
			area := image.Rect((x+padding)*scale, (y+padding)*scale, (x+padding+1)*scale, (y+padding+1)*scale)
			draw.Draw(canvas, area, image.NewUniform(color.RGBA{r, g, b, 255}), image.Point{}, draw.Src)
		}
	}
	file, err := os.Create(filepath.Join(filepath.Dir(path), "gopher-ccdp.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, canvas); err != nil {
		t.Fatal(err)
	}
}
