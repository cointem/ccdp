package agent

import (
	"bufio"
	"os"
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
		// ToolSearch success results contain "Name: <tool>".
		if i := strings.Index(line, `"Name: `); i > 0 {
			rest := line[i+len(`"Name: `):]
			if end := strings.IndexAny(rest, "\\\n"); end > 0 {
				rest = rest[:end]
			}
			if name := strings.TrimSpace(rest); name != "" {
				a.markDiscovered(name)
			}
		}
	}
}
