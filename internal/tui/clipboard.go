package tui

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/atotto/clipboard"
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
	// right-aligned toast on the hint row above the composer (chars is the
	// copied rune count) instead of the lane's left-side status/receipt text.
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
