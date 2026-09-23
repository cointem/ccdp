package tui

import "ccdp/internal/protocol"

// historyCell is one rendered entry in the conversation viewport.
type historyCell struct {
	revision uint64
	kind     string // user | command | assistant | system | welcome | tool | status | error | thinking
	text     string
	toolID   string
	status   string // running | success | error | denied
	// messageID is set for confirmed history entries. Local report entries use
	// reportAnchor/reportIndex to retain their position when a later snapshot
	// rebuilds the confirmed transcript.
	messageID string
	turnID    protocol.TurnID // associates transient errors with their durable turn outcome
	// reportID is a process-local stable identity for a local command/report.
	// It is separate from runtime message IDs so a report cannot collide with a
	// server-owned transcript message.
	reportID     string
	reportAnchor string
	reportIndex  int

	toolName      string
	toolArgs      map[string]any
	toolArgsRaw   string
	toolTruncated bool

	// agent carries the child sessions spawned by a Task/Agent tool call,
	// resolved by ParentCallID == toolID from the routing directory. It is
	// attached at compose time so both the managed live tail and the
	// native-scrollback flush render the same compact agent marker instead of
	// dumping the child's final answer as ordinary tool output.
	agent []protocol.ChildSession

	// Display cache compares source content, including equal-length corrections.
	sanitized       string
	sanitizedSource string
}

func (c *historyCell) cleanText() string {
	if c.sanitizedSource != c.text {
		c.sanitized = sanitizeANSI(c.text)
		c.sanitizedSource = c.text
	}
	return c.sanitized
}
