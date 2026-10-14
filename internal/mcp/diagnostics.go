package mcp

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"ccdp/internal/execution"
)

// boundedBuffer implements io.Writer for child diagnostics. It reports all
// input as consumed so an MCP process can never block on stderr after the
// retained diagnostic window fills; only the most recent limit bytes are kept.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		b.limit = maxMCPStderr
	}
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		b.truncated = true
		return len(p), nil
	}
	if remaining := b.limit - len(b.buf); len(p) > remaining {
		drop := len(p) - remaining
		b.buf = append(b.buf[drop:], p...)
		b.truncated = true
	} else {
		b.buf = append(b.buf, p...)
	}
	if len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := string(b.buf)
	if b.truncated {
		out += "\n[…MCP stderr truncated…]"
	}
	return out
}

var (
	mcpURLUserInfoPattern = regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`)
	mcpBearerPattern      = regexp.MustCompile(`(?i)(\bbearer\s+)[^\s,;]+`)
)

// mcpDiagnosticSuffix returns a bounded, redacted stderr suffix suitable for
// a startup/handshake error. Diagnostics are deliberately not logged from the
// background read loop: MCP servers may print credentials or private URLs.
func mcpDiagnosticSuffix(stderr *boundedBuffer, cfg ServerConfig) string {
	if stderr == nil {
		return ""
	}
	diagnostic := strings.TrimSpace(stderr.String())
	if diagnostic == "" {
		return ""
	}
	// Configured MCP environment is an explicit capability, but its values are
	// still secrets from the model's point of view. Redact every non-empty
	// configured value before returning diagnostics rather than trying to infer
	// whether a custom variable name is sensitive.
	for _, value := range cfg.Env {
		if value != "" {
			diagnostic = strings.ReplaceAll(diagnostic, value, "<redacted>")
		}
	}
	diagnostic = mcpURLUserInfoPattern.ReplaceAllString(diagnostic, `${1}<redacted>@`)
	diagnostic = mcpBearerPattern.ReplaceAllString(diagnostic, `${1}<redacted>`)
	const diagnosticLimit = 4 * 1024
	if len(diagnostic) > diagnosticLimit {
		diagnostic = "…" + diagnostic[len(diagnostic)-diagnosticLimit:]
	}
	return " (stderr: " + diagnostic + ")"
}

// mcpEnvironment removes inherited provider credentials while preserving
// explicit server-local environment from config. Explicit MCP env is a
// deliberate capability of that server, not an accidental leak from the
// host process.
func mcpEnvironment(extra map[string]string) []string {
	values := map[string]string{}
	for _, entry := range execution.SanitizedEnvironmentFor(execution.EnvironmentMCP, os.Environ()) {
		key, value := entry, ""
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key, value = entry[:i], entry[i+1:]
		}
		values[key] = value
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}
