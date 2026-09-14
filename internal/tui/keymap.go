package tui

import tea "github.com/charmbracelet/bubbletea"

// keymap.go keeps the non-modal key vocabulary in one place. Bubble Tea's
// KeyMsg.String values are stable across the terminal adapters we support, but
// centralizing the checks prevents reader, selector, and composer code from
// gradually assigning the same key to different actions.
type keyAction string

const (
	keyNone      keyAction = ""
	keyUp        keyAction = "up"
	keyDown      keyAction = "down"
	keyPageUp    keyAction = "page_up"
	keyPageDown  keyAction = "page_down"
	keyHome      keyAction = "home"
	keyEnd       keyAction = "end"
	keyEscape    keyAction = "escape"
	keyEnter     keyAction = "enter"
	keySearch    keyAction = "search"
	keyNextMatch keyAction = "next_match"
	keyRaw       keyAction = "raw"
	keyCopy      keyAction = "copy"
	keyQuit      keyAction = "quit"
	keyBackspace keyAction = "backspace"
)

func classifyKey(msg tea.KeyMsg) keyAction {
	switch msg.String() {
	case "up":
		return keyUp
	case "down":
		return keyDown
	case "pgup", "ctrl+u":
		return keyPageUp
	case "pgdown", "ctrl+d":
		return keyPageDown
	case "home":
		return keyHome
	case "end":
		return keyEnd
	case "esc":
		return keyEscape
	case "enter":
		return keyEnter
	case "/":
		return keySearch
	case "n":
		return keyNextMatch
	case "o":
		return keyRaw
	case "c":
		return keyCopy
	case "q":
		return keyQuit
	case "backspace", "ctrl+h":
		return keyBackspace
	default:
		return keyNone
	}
}
