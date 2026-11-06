package tools

import (
	"ccdp/internal/protocol"
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Output wakes the display, never the model. The tool result settles only
// after exit (including its final output drain) or invocation cancellation.
func (mp *managedProcess) waitForeground(ctx *Context) error {
	call := ctx.Context
	if call == nil {
		call = context.Background()
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var seen int64
	flush := func() {
		if ctx.ProcessProgress == nil {
			return
		}
		mp.mu.Lock()
		n := min(mp.total-seen, int64(mp.tail.Len()), 8192)
		raw := mp.tail.Tail(int(n))
		end := len(raw)
		if !mp.exited {
			for i := 0; i < len(raw); {
				if !utf8.FullRune(raw[i:]) {
					end = i
					break
				}
				_, width := utf8.DecodeRune(raw[i:])
				i += width
			}
		}
		text := string(raw[:end])
		omitted := mp.total-seen > n
		progress := protocol.ProcessProgress{ID: mp.identity.ID, Offset: mp.total - int64(len(raw)), Next: mp.total - int64(len(raw)-end), Truncated: omitted}
		seen = progress.Next
		mp.mu.Unlock()
		for len(text) > 0 && text[0]&0xc0 == 0x80 {
			text = text[1:]
			progress.Offset++
		}
		if omitted {
			text = "\n… additional output saved to the command log …\n" + text
		}
		if text != "" {
			ctx.ProcessProgress(progress, strings.ToValidUTF8(text, "�"))
		}
	}
	for {
		select {
		case <-call.Done():
			return call.Err()
		case <-mp.done:
			flush()
			mp.mu.Lock()
			err := mp.persistErr
			mp.mu.Unlock()
			return err
		case <-ticker.C:
			flush()
		}
	}
}

// Notifications carry a bounded tail even if retention already evicted the
// log. They do not compete with reads for an output cursor.
func (mp *managedProcess) completion() processView {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	v := mp.identity.ProcessOutput
	v.ResultID = "result." + v.ID
	v.Status, v.Log = "exited", mp.logPath
	if mp.reason != "" {
		v.Status = mp.reason
	}
	code := 0
	if mp.err != nil {
		code = 1
		if e, ok := mp.err.(interface{ ExitCode() int }); ok {
			code = e.ExitCode()
		}
	}
	v.ExitCode = &code
	raw := mp.tail.Tail(4096)
	start := 0
	for start < len(raw) && !utf8.RuneStart(raw[start]) {
		start++
	}
	v.Output = strings.ToValidUTF8(string(raw[start:]), "�")
	v.Offset, v.Next = int(mp.total)-len(raw)+start, int(mp.total)
	v.Truncated = v.Offset > 0
	if v.Truncated {
		v.Warning = "Showing the final output; use Read on log_path only if earlier output is needed."
	}
	if v.Log == "" {
		v.Warning = "The process handle and retained log have been released; this notification contains the final output preview."
	}
	if mp.total > mp.logSize {
		v.Warning += fmt.Sprintf(" %d output bytes were not retained in the log.", mp.total-mp.logSize)
	}
	return v
}
