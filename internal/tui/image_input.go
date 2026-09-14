package tui

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

type clipboardReadMsg struct {
	sessionID string
	request   protocol.CommandID
	content   ClipboardContent
	err       error
}

const foldedPasteLineThreshold = 8

// pasteFold retains the complete literal paste while the presentation layer
// may choose to show a compact line near the composer. Keeping the source here
// means expanding/editing never has to re-read the host clipboard.
type pasteFoldState struct {
	Original string
	Lines    int
	Expanded bool
}

func (p *pasteFoldState) summary() string {
	if p == nil || p.Lines <= foldedPasteLineThreshold {
		return ""
	}
	if p.Expanded {
		return fmt.Sprintf("pasted %d lines · expanded", p.Lines)
	}
	return fmt.Sprintf("pasted %d lines · ctrl+e to expand/edit", p.Lines)
}

func (m *Model) recordPastedText(text string) {
	// Count the complete editable source. A short paste appended to an
	// existing long paste must not leave a stale fold, and a paste after a
	// short draft may be what pushes the composer over the folding threshold.
	source := m.textarea.Value()
	if source == "" {
		source = text
	}
	lines := strings.Count(strings.ReplaceAll(source, "\r\n", "\n"), "\n") + 1
	if lines <= foldedPasteLineThreshold {
		m.pasteFold = nil
		return
	}
	m.pasteFold = &pasteFoldState{Original: source, Lines: lines}
}

func cloneInputImages(images []protocol.InputImage) []protocol.InputImage {
	return protocol.CloneInputImages(images)
}

func clonePasteFold(paste *pasteFoldState) *pasteFoldState {
	if paste == nil {
		return nil
	}
	copy := *paste
	return &copy
}

func (m *Model) togglePasteFold() bool {
	if m == nil || m.pasteFold == nil || m.pasteFold.Lines <= foldedPasteLineThreshold {
		return false
	}
	m.pasteFold.Expanded = !m.pasteFold.Expanded
	return true
}

func (m *Model) expandPasteFoldForEdit() {
	if m == nil || m.pasteFold == nil || m.pasteFold.Expanded {
		return
	}
	m.pasteFold.Expanded = true
}

func (m *Model) refreshPasteFold() {
	if m == nil || m.pasteFold == nil {
		return
	}
	source := m.textarea.Value()
	lines := strings.Count(strings.ReplaceAll(source, "\r\n", "\n"), "\n") + 1
	if lines <= foldedPasteLineThreshold {
		m.pasteFold = nil
		return
	}
	m.pasteFold.Original = source
	m.pasteFold.Lines = lines
}

func (m *Model) pasteClipboard() tea.Cmd {
	if m.resumePending {
		m.pushStatus("resume is opening; please wait")
		return nil
	}
	if m.pasteRequest != "" {
		m.pushStatus("reading clipboard…")
		return nil
	}
	reader := m.clipboardReader
	if reader == nil {
		reader = hostClipboard{}
	}
	m.pasteRequest = nextUICommandID()
	id, sessionID := m.pasteRequest, m.sessionID
	m.pushStatus("reading clipboard…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), clipboardReadTimeout)
		defer cancel()
		content, err := reader.ReadClipboard(ctx)
		return clipboardReadMsg{sessionID: sessionID, request: id, content: content, err: err}
	}
}

func (m *Model) applyClipboardRead(msg clipboardReadMsg) {
	if msg.request == "" || msg.request != m.pasteRequest {
		return
	}
	m.pasteRequest = ""
	if msg.err != nil {
		m.pushStatus("paste failed: " + msg.err.Error())
		return
	}
	if len(msg.content.Images) > 0 {
		images := append(append([]protocol.InputImage(nil), m.inputImages...), msg.content.Images...)
		if err := protocol.ValidateInputImages(images); err != nil {
			m.pushStatus("paste failed: " + err.Error())
			return
		}
		m.inputImages = append(m.inputImages, protocol.CloneInputImages(msg.content.Images)...)
		m.missingHistoryImages = false
		m.selectedImage = len(m.inputImages) - 1
		m.addNotice(Notice{Severity: NoticeSuccess, Text: fmt.Sprintf("%d image(s) attached", len(m.inputImages))})
		m.layout()
	} else if msg.content.Text != "" {
		// Insert as literal text; pasted /commands or newlines never submit.
		m.textarea.InsertString(msg.content.Text)
		m.recordPastedText(msg.content.Text)
		m.syncInputHeight()
		m.refreshCmdSuggest()
		m.pushStatus("text pasted")
	} else {
		m.pushStatus("clipboard has no image or text")
	}
}

func (m *Model) routeClipboardRead(msg clipboardReadMsg) {
	if msg.sessionID == m.sessionID {
		m.applyClipboardRead(msg)
		return
	}
	if m.routing != nil {
		if saved, ok := m.routing.views[msg.sessionID]; ok {
			saved.applyClipboardRead(msg)
			m.routing.views[msg.sessionID] = saved
		}
	}
}

func (m *Model) removeInputImage() {
	if len(m.inputImages) == 0 {
		return
	}
	i := min(max(0, m.selectedImage), len(m.inputImages)-1)
	images := append([]protocol.InputImage(nil), m.inputImages[:i]...)
	m.inputImages = append(images, m.inputImages[i+1:]...)
	m.selectedImage = max(0, min(i, len(m.inputImages)-1))
	m.pushStatus("image removed")
	m.layout()
}

func (m *Model) imageInputHeight() int {
	if len(m.inputImages) > 0 {
		return 1
	}
	return 0
}

func (m *Model) imageInputSummary() string {
	if len(m.inputImages) == 0 {
		return ""
	}
	idx := min(max(0, m.selectedImage), len(m.inputImages)-1)
	img := m.inputImages[idx]
	cfg, _ := protocol.InspectInputImage(img)
	hint := "alt+←/→ select · alt+backspace remove"
	if runtime.GOOS == "darwin" {
		hint = "⌥←/→ select · ⌥⌫ remove"
	}
	label := fmt.Sprintf("[Image %d/%d · %s · %d×%d · %d KB]  %s", idx+1, len(m.inputImages), strings.TrimPrefix(img.MediaType, "image/"), cfg.Width, cfg.Height, (len(img.Data)+1023)/1024, hint)
	return styleStatus.Render(truncateDisplay(label, max(1, m.width-2))) + "\n"
}

func sameInputImages(a, b []protocol.InputImage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].MediaType != b[i].MediaType || !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

// History keeps immutable attachments within a byte budget, not unbounded
// copies of every screenshot ever pasted. Ordinary text history is unchanged.
func (m *Model) rememberImageHistory(index int, images []protocol.InputImage) {
	if len(images) == 0 {
		return
	}
	if m.historyImages == nil {
		m.historyImages = make(map[int][]protocol.InputImage)
	}
	m.historyImages[index] = cloneInputImages(images)
	total := 0
	for i := len(m.history) - 1; i >= 0; i-- {
		for _, img := range m.historyImages[i] {
			total += len(img.Data)
		}
		if total > protocol.MaxInputImagesBytes {
			if len(m.historyImages[i]) > 0 {
				if m.expiredHistoryImages == nil {
					m.expiredHistoryImages = make(map[int]bool)
				}
				m.expiredHistoryImages[i] = true
			}
			delete(m.historyImages, i)
		}
	}
}

func (m *Model) recallHistoryImages() {
	m.pasteRequest = "" // a delayed read must not modify a recalled draft
	m.inputImages = cloneInputImages(m.historyImages[m.historyIdx])
	m.missingHistoryImages = m.expiredHistoryImages[m.historyIdx]
	if m.missingHistoryImages {
		m.pushStatus("older image attachments expired from input history; paste them again before sending")
	}
	m.selectedImage = max(0, len(m.inputImages)-1)
	m.layout()
}
