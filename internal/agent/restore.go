package agent

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// restoreDiscoveredFromTrace replays the session trace (JSONL) after a resume
// and re-marks deferred tools that were discovered via ToolSearch, so the
// resumed conversation keeps those tools available (Codex's rollout
// reconstruction idea).
func (a *Agent) restoreDiscoveredFromTrace() {
	path := a.TracePath()
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.Contains(line, `"tool":`) || !strings.Contains(line, `ToolSearch`) {
			continue
		}
		// ToolSearch success results contain "Name: <tool>". The trace line is
		// JSON-escaped, so the newline before "Name: " is the two bytes `\n`
		// — search for the unquoted form (a quoted form never occurs: the
		// quote would have to be part of the JSON string itself).
		if i := strings.Index(line, `Name: `); i > 0 {
			rest := line[i+len(`Name: `):]
			// Cut at the JSON escape of the trailing newline (backslash) or at
			// any quote/backslash; `end >= 0` so an empty name never slips the
			// whole rest of the line through.
			if end := strings.IndexAny(rest, `\"`+"\n"); end >= 0 {
				rest = rest[:end]
			}
			if name := strings.TrimSpace(rest); isToolNameToken(name) {
				a.markDiscovered(name)
			}
		}
	}
}

// isToolNameToken validates a candidate tool name recovered from a trace line.
var toolNameTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)?$`)

func isToolNameToken(s string) bool {
	return len(s) > 0 && len(s) <= 128 && toolNameTokenRe.MatchString(s)
}
