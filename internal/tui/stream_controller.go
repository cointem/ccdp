package tui

import (
	"github.com/rivo/uniseg"
	"strings"
	"unicode/utf8"
)

// StreamController plans a stable source prefix. It has no terminal effects;
// the history ledger advances its offset only after a batch acknowledgment.
type StreamController struct{}

func (StreamController) CommitEnd(source string, offset, width int, identified bool) int {
	if offset < 0 || offset > len(source) {
		return 0
	}
	if !streamChunkReady(source, offset) && !(offset == 0 && identified) {
		return offset
	}
	end := markdownStablePrefixEnd(source, offset, max(1, width))
	if end < offset || end > len(source) {
		return offset
	}
	return end
}

// streamCommitEnd commits whole grapheme clusters only. The final cluster is
// retained until another arrives, because a later combining mark or ZWJ can
// extend it. Newlines are explicit stable boundaries.
func streamCommitEnd(text string, width int) int {
	width = max(1, width)
	last, column := 0, 0
	clusters := uniseg.NewGraphemes(text)
	for clusters.Next() {
		start, end := clusters.Positions()
		cluster := clusters.Str()
		if !utf8.ValidString(cluster) {
			break
		}
		if cluster == "\n" || cluster == "\r\n" {
			last = end
			column = 0
			continue
		}
		w := uniseg.StringWidth(cluster)
		if column > 0 && column+w > width {
			last = start
			column = 0
		}
		column += w
		if column >= width && end < len(text) {
			last = end
			column = 0
		}
	}
	return last
}

func markdownStablePrefixEnd(source string, offset, width int) int {
	if offset < 0 || offset > len(source) {
		offset = 0
	}
	if offset >= len(source) {
		return offset
	}
	// Keep an unclosed fence in the managed frame. Once the fence closes, the
	// complete code block is safe to freeze as one display unit.
	lastSafe := -1
	inFence := false
	lineStart := offset
	for lineStart < len(source) {
		lineEnd := strings.IndexByte(source[lineStart:], '\n')
		if lineEnd < 0 {
			break
		}
		lineEnd += lineStart + 1
		line := strings.TrimSpace(source[lineStart : lineEnd-1])
		if markdownFenceRE.MatchString(source[lineStart : lineEnd-1]) {
			inFence = !inFence
			if !inFence {
				lastSafe = lineEnd
			}
		} else if !inFence && line == "" {
			lastSafe = lineEnd
		}
		lineStart = lineEnd
	}
	if lastSafe > offset {
		return lastSafe
	}
	// Long plain output still needs bounded streaming. Visual boundaries are a
	// conservative fallback only when the current suffix has no Markdown
	// markers. A pending list/table/emphasis block must stay in the managed tail
	// until its source proves that the block is complete.
	if !inFence && !markdownHasPendingSyntax(source[offset:]) {
		return offset + streamCommitEnd(source[offset:], width)
	}
	return offset
}

func markdownHasPendingSyntax(source string) bool {
	if strings.Count(source, "```")%2 == 1 ||
		strings.Count(source, "**")%2 == 1 ||
		strings.Count(source, "__")%2 == 1 ||
		strings.Count(source, "~~")%2 == 1 {
		return true
	}
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "|") || markdownListRE.MatchString(line) || markdownHeadingRE.MatchString(line) || strings.HasPrefix(trimmed, ">") {
			return true
		}
	}
	return false
}

const (
	inlinePrefixBytes = 2048
	inlinePrefixLines = 16
)

func streamChunkReady(text string, offset int) bool {
	if offset < 0 || offset > len(text) {
		offset = 0
	}
	delta := text[offset:]
	return len(delta) >= inlinePrefixBytes || strings.Count(delta, "\n") >= inlinePrefixLines
}
