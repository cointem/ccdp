package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
)

// Clipboard is the narrow client-side clipboard boundary. The default
// implementation delegates to the host adapter (pbcopy on macOS), while tests
// and non-macOS frontends can provide an in-memory writer. Ctrl/Cmd-C is never
// routed here: Ctrl-C remains interrupt/quit and Cmd-C remains native terminal
// selection copy.
type Clipboard interface {
	WriteAll(string) error
}

type hostClipboard struct{}

func (hostClipboard) WriteAll(text string) error { return clipboard.WriteAll(text) }

var clipboardWriteTimeout = 2 * time.Second

// clipboardResultMsg is tagged with both session and report generations. A
// slow host pasteboard operation must not post a success/failure notice into a
// session that the user has already resumed or switched to.
type clipboardResultMsg struct {
	sessionID  string
	generation uint64
	err        error
}

// contextClipboard is an optional cancellable extension implemented by the
// macOS adapter. Test and third-party adapters may keep the small legacy
// Clipboard interface; writeClipboard still bounds their operation.
type contextClipboard interface {
	WriteAllContext(context.Context, string) error
}

// WriteAllContext uses pbcopy directly on macOS so a hung pasteboard process
// can be terminated by the command context. Other hosts retain atotto's
// portable adapter through WriteAll.
func (hostClipboard) WriteAllContext(ctx context.Context, text string) error {
	if runtime.GOOS != "darwin" {
		result := make(chan error, 1)
		go func() { result <- clipboard.WriteAll(text) }()
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	cmd := exec.CommandContext(ctx, "pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func writeClipboard(ctx context.Context, writer Clipboard, text string) error {
	if cancellable, ok := writer.(contextClipboard); ok {
		return cancellable.WriteAllContext(ctx, text)
	}
	// The historical Clipboard interface cannot accept a context. Keep its
	// result channel buffered so a legacy adapter that returns after the timeout
	// does not strand a sender or block shutdown.
	result := make(chan error, 1)
	go func() { result <- writer.WriteAll(text) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Model) responseText(id string) (string, bool) {
	find := func(items []logItem) (string, bool) {
		for i := len(items) - 1; i >= 0; i-- {
			item := items[i]
			if item.kind != "assistant" || item.text == "" {
				continue
			}
			if id == "" || item.messageID == id || item.reportID == id {
				return item.text, true
			}
		}
		return "", false
	}
	if text, ok := find(m.confirmedItems); ok {
		return text, true
	}
	return find(m.items)
}

// copyLatestResponse starts a bounded host clipboard operation. With an
// optional stable message ID it copies that response; no ID retains the
// familiar latest-response behavior.
func (m *Model) copyLatestResponse(id ...string) tea.Cmd {
	selectedID := ""
	if len(id) > 0 {
		selectedID = id[0]
	}
	text, ok := m.responseText(selectedID)
	if !ok {
		if selectedID == "" {
			m.pushStatus("nothing to copy")
		} else {
			m.pushStatus(fmt.Sprintf("no assistant response with id %q", selectedID))
		}
		return nil
	}
	writer := m.clipboard
	if writer == nil {
		writer = hostClipboard{}
	}
	sessionID, generation := m.sessionID, m.reportGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), clipboardWriteTimeout)
		defer cancel()
		err := writeClipboard(ctx, writer, text)
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("clipboard write timed out after %s", clipboardWriteTimeout)
		}
		return clipboardResultMsg{sessionID: sessionID, generation: generation, err: err}
	}
}
