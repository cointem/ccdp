package shellsyntax

import (
	"path/filepath"
	"strings"
)

// Executable unwraps supported static command wrappers. An unknown wrapper
// option returns false so callers retain their conservative fallback policy.
func Executable(words []string) ([]string, bool) {
	for len(words) > 0 {
		if name, _, ok := strings.Cut(words[0], "="); ok && validName(name) {
			words = words[1:]
			continue
		}
		switch filepath.Base(words[0]) {
		case "env":
			words = words[1:]
			for len(words) > 0 && strings.HasPrefix(words[0], "-") {
				switch words[0] {
				case "--":
					words = words[1:]
					goto next
				case "-i", "--ignore-environment":
					words = words[1:]
				case "-u", "--unset":
					if len(words) < 2 {
						return nil, false
					}
					words = words[2:]
				default:
					return nil, false
				}
			}
		case "command", "exec", "sudo":
			wrapper := filepath.Base(words[0])
			words = words[1:]
			if len(words) > 0 && words[0] == "--" {
				words = words[1:]
			}
			if len(words) > 0 && strings.HasPrefix(words[0], "-") {
				// command -v/-V describes a name rather than executing it.
				if wrapper == "command" && (words[0] == "-v" || words[0] == "-V") {
					return []string{"command"}, true
				}
				return nil, false
			}
		default:
			return words, true
		}
	next:
	}
	return nil, true
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// ShellProgram identifies the fixed -c script of an explicitly invoked shell.
func ShellProgram(words []string) (string, bool) {
	if len(words) < 3 {
		return "", false
	}
	switch filepath.Base(words[0]) {
	case "sh", "bash", "zsh", "dash", "ksh":
	default:
		return "", false
	}
	for i := 1; i < len(words)-1; i++ {
		if strings.HasPrefix(words[i], "-") && !strings.HasPrefix(words[i], "--") && strings.Contains(words[i][1:], "c") {
			return words[i+1], true
		}
	}
	return "", false
}
