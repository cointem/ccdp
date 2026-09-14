package commands

import (
	"fmt"
	"strings"
	"unicode"
)

// ParseLine parses a command line without invoking a shell. It handles the
// quoting users expect for slash-command paths and messages while keeping
// shell metacharacters inert. The parser is shared by every interactive
// client so aliases and custom commands see the same argument boundaries.
func ParseLine(input string) ([]string, error) {
	var fields []string
	var current strings.Builder
	var quote rune
	escaped := false
	haveValue := false
	flush := func() {
		if haveValue {
			fields = append(fields, current.String())
			current.Reset()
			haveValue = false
		}
	}
	for _, r := range input {
		if escaped {
			current.WriteRune(r)
			haveValue = true
			escaped = false
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' {
				escaped = true
			} else {
				current.WriteRune(r)
				haveValue = true
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
			haveValue = true
		case r == '\\':
			escaped = true
			haveValue = true
		case unicode.IsSpace(r):
			flush()
		default:
			current.WriteRune(r)
			haveValue = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	flush()
	return fields, nil
}
