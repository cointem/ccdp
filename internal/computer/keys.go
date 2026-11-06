package computer

import (
	"fmt"
	"strings"
)

// macOS virtual key codes. Chords press and release within one native call;
// there is deliberately no persistent key-down or button-down operation.
var keyCodes = map[string]uint16{
	"a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9, "b": 11,
	"q": 12, "w": 13, "e": 14, "r": 15, "y": 16, "t": 17, "1": 18, "2": 19, "3": 20, "4": 21,
	"6": 22, "5": 23, "=": 24, "9": 25, "7": 26, "-": 27, "8": 28, "0": 29, "]": 30,
	"o": 31, "u": 32, "[": 33, "i": 34, "p": 35, "enter": 36, "l": 37, "j": 38, "'": 39,
	"k": 40, ";": 41, "\\": 42, ",": 43, "/": 44, "n": 45, "m": 46, ".": 47,
	"tab": 48, "space": 49, "`": 50, "backspace": 51, "escape": 53,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97, "f7": 98, "f8": 100,
	"f9": 101, "f10": 109, "f11": 103, "f12": 111,
	"home": 115, "end": 119, "pageup": 116, "pagedown": 121, "delete": 117,
	"left": 123, "right": 124, "down": 125, "up": 126,
}

func parseKey(key string) (uint16, uint64, error) {
	parts := strings.Split(strings.ToLower(key), "+")
	var flags uint64
	for _, part := range parts[:len(parts)-1] {
		switch part {
		case "cmd":
			flags |= 1 << 20
		case "shift":
			flags |= 1 << 17
		case "alt":
			flags |= 1 << 19
		case "ctrl":
			flags |= 1 << 18
		default:
			return 0, 0, fmt.Errorf("unknown key modifier %q", part)
		}
	}
	code, ok := keyCodes[parts[len(parts)-1]]
	if !ok {
		return 0, 0, fmt.Errorf("unsupported key %q", key)
	}
	return code, flags, nil
}
