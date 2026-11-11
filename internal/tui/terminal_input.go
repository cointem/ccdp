package tui

import (
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Bubble Tea v1 treats short reads as event boundaries. A split SGR mouse
// report can therefore arrive as Escape/Alt+[ followed by ordinary KeyRunes.
// Repair those fragments before any panel or composer sees them. Literal text
// and bracketed pastes never start a repair; a real ESC prefix is required.
// The timeout preserves standalone Escape and Alt+[ without waiting for
// another key. All state is owned by the program's event loop, across views.
const terminalEscapeWait = 100 * time.Millisecond

type terminalInputTimeoutMsg uint64
type terminalInputBatchMsg []tea.Msg

type terminalInput struct {
	prefix string
	keys   []tea.Msg
	serial uint64
	timer  *time.Timer
	send   func(tea.Msg)
}

func (in *terminalInput) filter(_ tea.Model, msg tea.Msg) tea.Msg {
	if timeout, ok := msg.(terminalInputTimeoutMsg); ok {
		if uint64(timeout) != in.serial {
			return nil
		}
		return inputMessages(in.flush())
	}
	key, isKey := msg.(tea.KeyMsg)
	if !isKey {
		// Render/watch/tick messages must not break a partially read event.
		if _, mouse := msg.(tea.MouseMsg); mouse {
			return inputMessages(append(in.flush(), msg))
		}
		return msg
	}
	if in.prefix != "" {
		if !key.Paste && !key.Alt && key.Type == tea.KeyRunes {
			candidate := in.prefix + string(key.Runes)
			mouse, consumed, partial := parseMouseReport(candidate)
			if partial {
				in.prefix = candidate
				in.keys = append(in.keys, key)
				in.wait()
				return nil
			}
			if consumed > 0 {
				in.flush()
				messages := []tea.Msg{mouse}
				if tail := candidate[consumed:]; tail != "" {
					messages = append(messages, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tail)})
				}
				return inputMessages(messages)
			}
		}
		// An incomplete or non-mouse escape is ordinary keyboard input. Replay
		// the original keys in order, and let this key start a new prefix.
		messages := in.flush()
		if next := in.filter(nil, key); next != nil {
			messages = append(messages, next)
		}
		return inputMessages(messages)
	}
	if !key.Paste {
		switch {
		case key.Type == tea.KeyEsc && !key.Alt:
			in.prefix = "\x1b"
		case key.Type == tea.KeyRunes && key.Alt && string(key.Runes) == "[":
			in.prefix = "\x1b["
		}
	}
	if in.prefix == "" {
		return key
	}
	in.keys = []tea.Msg{key}
	in.wait()
	return nil
}

func (in *terminalInput) wait() {
	if in.timer != nil {
		in.timer.Stop()
	}
	in.serial++
	serial := in.serial
	in.timer = time.AfterFunc(terminalEscapeWait, func() { in.send(terminalInputTimeoutMsg(serial)) })
}

func (in *terminalInput) flush() []tea.Msg {
	if in.timer != nil {
		in.timer.Stop()
		in.timer = nil
	}
	in.serial++ // Ignore a timeout already queued before Stop.
	keys := in.keys
	in.prefix, in.keys = "", nil
	return keys
}

func inputMessages(messages []tea.Msg) tea.Msg {
	switch len(messages) {
	case 0:
		return nil
	case 1:
		return messages[0]
	default:
		return terminalInputBatchMsg(messages)
	}
}

// parseMouseReport recognizes only ESC [ < button ; column ; row M/m. Bound
// each numeric field so malformed input cannot retain an unbounded key buffer.
// consumed allows a report and subsequent typed text to share one KeyMsg.
func parseMouseReport(s string) (mouse tea.MouseMsg, consumed int, partial bool) {
	const prefix = "\x1b[<"
	if len(s) < len(prefix) {
		return mouse, 0, strings.HasPrefix(prefix, s)
	}
	if !strings.HasPrefix(s, prefix) {
		return mouse, 0, false
	}
	var values [3]int
	i := len(prefix)
	for field := range values {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i-start > 10 {
			return mouse, 0, false
		}
		if i == len(s) {
			return mouse, 0, true
		}
		value, err := strconv.ParseUint(s[start:i], 10, 31)
		if err != nil {
			return mouse, 0, false
		}
		values[field] = int(value)
		if field < 2 {
			if s[i] != ';' {
				return mouse, 0, false
			}
			i++
		}
	}
	if s[i] != 'M' && s[i] != 'm' || values[0] > 255 || values[1] == 0 || values[2] == 0 {
		return mouse, 0, false
	}
	code := values[0]
	mouse.X, mouse.Y = values[1]-1, values[2]-1
	mouse.Shift, mouse.Alt, mouse.Ctrl = code&4 != 0, code&8 != 0, code&16 != 0
	switch {
	case code&128 != 0:
		mouse.Button = tea.MouseButtonBackward + tea.MouseButton(code&3)
	case code&64 != 0:
		mouse.Button = tea.MouseButtonWheelUp + tea.MouseButton(code&3)
	case code&3 == 3:
		mouse.Button, mouse.Action = tea.MouseButtonNone, tea.MouseActionRelease
	default:
		mouse.Button = tea.MouseButtonLeft + tea.MouseButton(code&3)
	}
	wheel := tea.MouseEvent(mouse).IsWheel()
	if code&32 != 0 && !wheel {
		mouse.Action = tea.MouseActionMotion
	} else if s[i] == 'm' && !wheel {
		mouse.Action = tea.MouseActionRelease
	}
	// Preserve the deprecated Type field too, matching native Bubble Tea events.
	types := [...]tea.MouseEventType{tea.MouseUnknown, tea.MouseLeft, tea.MouseMiddle, tea.MouseRight,
		tea.MouseWheelUp, tea.MouseWheelDown, tea.MouseWheelLeft, tea.MouseWheelRight, tea.MouseBackward, tea.MouseForward}
	if int(mouse.Button) < len(types) {
		mouse.Type = types[mouse.Button]
	}
	if mouse.Action == tea.MouseActionRelease {
		mouse.Type = tea.MouseRelease
	} else if mouse.Action == tea.MouseActionMotion && mouse.Type == tea.MouseUnknown {
		mouse.Type = tea.MouseMotion
	}
	return mouse, i + 1, false
}
