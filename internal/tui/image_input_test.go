package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"ccdp/internal/protocol"
	tea "github.com/charmbracelet/bubbletea"
)

func clipboardPNG(t *testing.T) protocol.InputImage {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return protocol.InputImage{MediaType: "image/png", Data: b.Bytes()}
}

type fakeClipboardReader struct {
	content ClipboardContent
	err     error
}

func (r fakeClipboardReader) ReadClipboard(context.Context) (ClipboardContent, error) {
	return r.content, r.err
}

func pasteTestImage(t *testing.T, m *Model) {
	t.Helper()
	m.clipboardReader = fakeClipboardReader{content: ClipboardContent{Images: []protocol.InputImage{clipboardPNG(t)}}}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlV})
	if cmd == nil {
		t.Fatal("missing clipboard command")
	}
	m.applyClipboardRead(cmd().(clipboardReadMsg))
}

func TestImagePasteSubmitRetryAndHistory(t *testing.T) {
	m, d := routingTestModel()
	pasteTestImage(t, &m)
	if len(m.inputImages) != 1 || !strings.Contains(m.renderInput(), "2×3") {
		t.Fatal("image not visible in composer")
	}
	_, cmd := m.submit()
	if cmd == nil {
		t.Fatal("image-only submit rejected")
	}
	_ = cmd()
	sent := d.clients["root"].submits[0]
	if len(sent.Input.Images) != 1 || sent.Input.Text != "" {
		t.Fatal("wrong typed input")
	}
	if len(m.inputImages) != 0 {
		t.Fatal("submitted attachments not cleared")
	}
	m.restoreFailedSubmission(sent.ID)
	if len(m.inputImages) != 1 {
		t.Fatal("failed image-only intent lost")
	}
	_, retry := m.submit()
	_ = retry()
	sentAgain := d.clients["root"].submits[1]
	if !reflect.DeepEqual(sent, sentAgain) {
		t.Fatal("retry changed command/input identity or bytes")
	}
	m.applyReceipt(protocol.Receipt{CommandID: sent.ID, Status: protocol.ReceiptApplied}, "")
	if m.pendingSubmissionCount() != 0 {
		t.Fatal("successful send retained pending images")
	}
	m.historyPrev()
	if len(m.inputImages) != 1 {
		t.Fatal("history lost images")
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyBackspace, Alt: true})
	if len(m.inputImages) != 0 {
		t.Fatal("image delete failed")
	}
}

func TestImagePasteRoutingAndStaleRead(t *testing.T) {
	m, _ := routingTestModel()
	m.clipboardReader = fakeClipboardReader{content: ClipboardContent{Images: []protocol.InputImage{clipboardPNG(t)}}}
	read := m.pasteClipboard()
	if _, cmd := m.submit(); cmd != nil {
		t.Fatal("sent while clipboard is pending")
	}
	m = switchRoutingTest(t, m, "child")
	next, _ := m.Update(read())
	m = modelValue(t, next)
	if len(m.inputImages) != 0 {
		t.Fatal("root paste polluted child")
	}
	m = switchRoutingTest(t, m, "root")
	if len(m.inputImages) != 1 {
		t.Fatal("background paste lost")
	}
	m = switchRoutingTest(t, m, "child")
	m = switchRoutingTest(t, m, "root")
	if len(m.inputImages) != 1 {
		t.Fatal("navigation lost image draft")
	}
	read = m.pasteClipboard()
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m.applyClipboardRead(read().(clipboardReadMsg))
	if len(m.inputImages) != 1 {
		t.Fatal("escape should preserve the existing attachment while rejecting a stale read")
	}
}

func TestLongTextPasteFoldsWithoutLosingEditableSource(t *testing.T) {
	m, _ := routingTestModel()
	m.width, m.height = 80, 24
	m.layout()
	source := strings.Join([]string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine"}, "\n")
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(source), Paste: true})
	if m.textarea.Value() != source || m.pasteFold == nil || m.pasteFold.Expanded {
		t.Fatalf("long paste was not retained as a collapsed editable source: value=%q fold=%+v", m.textarea.Value(), m.pasteFold)
	}
	if got := m.desiredInputHeight(); got != 1 {
		t.Fatalf("collapsed paste reserved %d composer rows, want 1", got)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlE})
	if m.pasteFold == nil || !m.pasteFold.Expanded || m.desiredInputHeight() <= 1 {
		t.Fatalf("Ctrl+E did not expand the editable paste: fold=%+v height=%d", m.pasteFold, m.desiredInputHeight())
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'!'}})
	if !strings.Contains(m.textarea.Value(), "!") || m.pasteFold == nil || !m.pasteFold.Expanded {
		t.Fatalf("expanded paste edit lost source or collapsed unexpectedly: value=%q fold=%+v", m.textarea.Value(), m.pasteFold)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlE})
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if m.pasteFold == nil || !m.pasteFold.Expanded || !strings.Contains(m.textarea.Value(), "?") {
		t.Fatal("editing a folded paste did not reveal and retain the source")
	}
}

func TestImagePasteFailureAndTextFallback(t *testing.T) {
	m, _ := routingTestModel()
	m.clipboardReader = fakeClipboardReader{content: ClipboardContent{Text: "hello\n/world"}}
	read := m.pasteClipboard()
	m.applyClipboardRead(read().(clipboardReadMsg))
	if m.textarea.Value() != "hello\n/world" || m.pendingSubmissionCount() > 0 {
		t.Fatal("text paste was not literal")
	}
	pasteTestImage(t, &m)
	m.clipboardReader = fakeClipboardReader{err: errors.New("clipboard unavailable")}
	read = m.pasteClipboard()
	m.applyClipboardRead(read().(clipboardReadMsg))
	if len(m.inputImages) != 1 || !strings.Contains(noticeText(&m), "paste failed") {
		t.Fatal("failed paste altered draft")
	}
	_, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ordinary paste"), Paste: true})
	if m.pasteRequest != "" {
		t.Fatal("ordinary text paste read system clipboard")
	}
}

func TestImagePasteLimitsAndFailedDraftIsolation(t *testing.T) {
	m, _ := routingTestModel()
	for range protocol.MaxInputImages {
		pasteTestImage(t, &m)
	}
	pasteTestImage(t, &m)
	if len(m.inputImages) != protocol.MaxInputImages || !strings.Contains(noticeText(&m), "at most") {
		t.Fatal("image count not bounded")
	}
	_, _ = m.submit()
	pasteTestImage(t, &m)
	for id, op := range m.operations {
		if !op.Recoverable {
			continue
		}
		m.restoreFailedSubmission(id)
		if len(m.inputImages) != 1 || len(m.retryImages) != protocol.MaxInputImages {
			t.Fatal("failure overwrote new image draft")
		}
	}
}

func TestImageHistoryBudgetDoesNotSendWithoutImages(t *testing.T) {
	m, _ := routingTestModel()
	m.history = []string{"old picture", "newer picture", "latest picture"}
	m.historyIdx = 0
	data := make([]byte, protocol.MaxInputImageBytes)
	copy(data, clipboardPNG(t).Data)
	for i := range 3 {
		m.rememberImageHistory(i, []protocol.InputImage{{MediaType: "image/png", Data: data}})
	}
	m.textarea.SetValue(m.history[0])
	m.recallHistoryImages()
	if !m.missingHistoryImages || len(m.historyImages) != 2 {
		t.Fatal("history budget not enforced")
	}
	if _, cmd := m.submit(); cmd != nil {
		t.Fatal("old prompt sent without its expired images")
	}
	pasteTestImage(t, &m)
	if m.missingHistoryImages {
		t.Fatal("replacement image did not unblock draft")
	}
}

func TestClipboardPlatformAdapters(t *testing.T) {
	img := clipboardPNG(t)
	for _, platform := range []string{"darwin", "windows", "linux-wsl"} {
		t.Run(platform, func(t *testing.T) {
			calls := 0
			run := func(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
				calls++
				if name != "/usr/bin/osascript" && name != "powershell.exe" {
					t.Fatal(name)
				}
				return []byte(base64.StdEncoding.EncodeToString(img.Data)), nil
			}
			content, err := readHostClipboard(context.Background(), platform, platform == "linux-wsl", false, run)
			if err != nil || !sameInputImages(content.Images, []protocol.InputImage{img}) || calls != 1 {
				t.Fatalf("read: %v %d", err, calls)
			}
		})
	}
	for _, wayland := range []bool{false, true} {
		calls := 0
		run := func(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
			calls++
			if wayland && name != "wl-paste" || !wayland && name != "xclip" {
				t.Fatal(name)
			}
			if calls == 1 {
				return []byte("text/plain\nimage/png\n"), nil
			}
			if !strings.Contains(strings.Join(args, " "), "image/png") {
				t.Fatal(args)
			}
			return img.Data, nil
		}
		content, err := readHostClipboard(context.Background(), "linux", false, wayland, run)
		if err != nil || len(content.Images) != 1 {
			t.Fatal(err)
		}
	}
}

func TestClipboardCommandBoundsAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper fixtures")
	}
	if _, err := runClipboardCommand(context.Background(), 2, "printf", "abc"); err == nil {
		t.Fatal("output limit not enforced")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runClipboardCommand(ctx, 100, "sleep", "5"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("clipboard read blocked terminal")
	}
}

// This opt-in integration test uses an isolated pasteboard, never reads or
// changes the user's general clipboard, and does not require a model/API.
func TestNativeMacImageClipboard(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("CCDP_TEST_NATIVE_CLIPBOARD") != "1" {
		t.Skip("opt-in macOS pasteboard integration")
	}
	img := clipboardPNG(t)
	for _, kind := range []string{"png", "tiff", "file", "empty"} {
		t.Run(kind, func(t *testing.T) {
			setup := `var pb = $.NSPasteboard.pasteboardWithUniqueName; var fixture = $.NSData.alloc.initWithBase64EncodedStringOptions('` + base64.StdEncoding.EncodeToString(img.Data) + `',0);`
			switch kind {
			case "png":
				setup += `pb.setDataForType(fixture,'public.png');`
			case "tiff":
				setup += `var rep = $.NSBitmapImageRep.imageRepWithData(fixture); var tiff = rep.representationUsingTypeProperties($.NSTIFFFileType, $.NSDictionary.dictionary); pb.setDataForType(tiff,'public.tiff');`
			case "file":
				path := filepath.Join(t.TempDir(), "copied image.png")
				if err := os.WriteFile(path, img.Data, 0600); err != nil {
					t.Fatal(err)
				}
				encodedURL, _ := json.Marshal((&url.URL{Scheme: "file", Path: path}).String())
				setup += `pb.setStringForType(` + string(encodedURL) + `,'public.file-url');`
			}
			script := strings.Replace(macClipboardImageScript, "var pb = $.NSPasteboard.generalPasteboard;", setup, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			encoded, err := runClipboardCommand(ctx, 1<<20, "/usr/bin/osascript", "-l", "JavaScript", "-e", script)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "empty" {
				if len(bytes.TrimSpace(encoded)) != 0 {
					t.Fatal("empty clipboard became an image")
				}
				return
			}
			data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := protocol.InspectInputImage(protocol.InputImage{MediaType: "image/png", Data: data})
			if err != nil || cfg.Width != 2 || cfg.Height != 3 {
				t.Fatalf("native conversion: %+v %v", cfg, err)
			}
			if kind == "png" && !bytes.Equal(data, img.Data) {
				t.Fatal("native PNG read differs")
			}
		})
	}
}
