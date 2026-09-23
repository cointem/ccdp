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
	WriteAllContext(context.Context, string) error
}

type hostClipboard struct{}

var clipboardWriteTimeout = 2 * time.Second

// clipboardResultMsg is tagged with both session and report generations. A
// slow host pasteboard operation must not post a success/failure notice into a
// session that the user has already resumed or switched to.
type clipboardResultMsg struct {
	sessionID  string
	generation uint64
	err        error
	// selection marks a drag-copy result. Its success is surfaced through the
	// right-aligned toast above the composer (chars is the copied rune count)
	// instead of a status-row notice, so the transcript never shifts.
	selection bool
	chars     int
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

func (m *Model) responseText(id string) (string, bool) {
	find := func(items []historyCell) (string, bool) {
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
		err := writer.WriteAllContext(ctx, text)
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("clipboard write timed out after %s", clipboardWriteTimeout)
		}
		return clipboardResultMsg{sessionID: sessionID, generation: generation, err: err}
	}
}
