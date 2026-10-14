package shellsyntax

import "strings"

// shellSegment retains both the rule-matching spelling and literal argv. Both
// default classification and persistent rules use this analysis, so quoting
// cannot hide execution from one gate while the other sees only a prefix.
// This is a deliberately limited shell grammar, not a shell interpreter.
// Constructs requiring expansion or redirection require explicit approval.
type Segment struct {
	Text  string
	Words []string
}

func Analyze(command string) ([]Segment, bool) {
	var segments []Segment
	var words []string
	var word strings.Builder
	started := false
	quote := byte(0)
	start := 0
	flushWord := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	flushSegment := func(end int) {
		flushWord()
		if len(words) > 0 {
			segments = append(segments, Segment{strings.TrimSpace(command[start:end]), words})
			words = nil
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == 0 || c == '\r' {
			return nil, false
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if c == '\\' {
			if i+1 == len(command) {
				return nil, false
			}
			next := command[i+1]
			// POSIX double quotes preserve backslashes before ordinary characters.
			if quote == '"' && !strings.ContainsRune("$`\"\\\n", rune(next)) {
				word.WriteByte(c)
				continue
			}
			i++
			if next != '\n' {
				started = true
				word.WriteByte(next)
			}
			continue
		}
		// Double quotes do not suppress shell expansion. Reject parameter,
		// arithmetic and command expansion together instead of approximating them.
		if c == '$' || c == '`' {
			return nil, false
		}
		if quote == '"' {
			if c == '"' {
				quote = 0
			} else {
				word.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			started = true
		case ' ', '\t':
			flushWord()
		case ';', '\n', '&', '|':
			flushSegment(i)
			if i+1 < len(command) && (c == '&' || c == '|') && command[i+1] == c {
				i++
			}
			start = i + 1
		// Unquoted wildcards may expand to option-looking filenames. Shell
		// grammar, redirections and comments cannot safely be treated as argv.
		case '<', '>', '(', ')', '{', '}', '*', '?', '[', '~', '#':
			return nil, false
		default:
			started = true
			word.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, false
	}
	flushSegment(len(command))
	return segments, len(segments) > 0
}
