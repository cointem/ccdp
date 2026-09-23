package tui

import (
	"context"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// linkTarget is a clickable span of an OSC 8 hyperlink label: row indexes the
// transcript content row (the same numbering clickTargets and the selection
// engine use) and [start, end) are display-cell columns within that row.
type linkTarget struct {
	row    int
	start  int
	end    int
	target string
}

// Only generate terminal controls from validated destinations, after untrusted
// source controls have been removed by the Markdown parser.
func terminalLinkTarget(target string, workspace ...string) string {
	target = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">"))
	if target == "" || strings.IndexFunc(target, unicode.IsControl) >= 0 {
		return ""
	}
	if filepath.IsAbs(target) {
		return (&url.URL{Scheme: "file", Path: filepath.Clean(target)}).String()
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return ""
	}
	if parsed.Scheme == "" && parsed.Host == "" && len(workspace) > 0 && filepath.IsAbs(workspace[0]) && parsed.Path != "" {
		parsed.Scheme = "file"
		parsed.Path = filepath.Join(workspace[0], parsed.Path)
		return parsed.String()
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http":
		if parsed.Host != "" {
			return parsed.String()
		}
	case "file":
		if strings.HasPrefix(parsed.Path, "/") {
			return parsed.String()
		}
	}
	return ""
}

func renderTerminalLink(label, target string, workspace ...string) string {
	visible := styleMarkdownLink.Render(label)
	if destination := terminalLinkTarget(target, workspace...); destination != "" {
		return osc8Prefix + destination + osc8Terminator + visible + closeOSC8
	}
	return visible + styleStatus.Render(" ("+sanitizeANSI(target)+")")
}

const (
	osc8Prefix     = "\x1b]8;;"
	osc8Terminator = "\x1b\\"
	closeOSC8      = "\x1b]8;;\x1b\\"
	osc8Open       = "\x1b]8;"
)

// linkScanner recovers the clickable spans of OSC 8 hyperlinks from rendered
// rows. The TUI enables mouse reporting, so the terminal never sees the click
// that would open a hyperlink; the transcript must resolve link clicks itself,
// which requires knowing which cells of which row carry a destination.
//
// The scanner is stateful because a wrapped label can open on one row and close
// on a later one: the destination stays pending across rows until the closing
// sequence arrives.
type linkScanner struct {
	pending string
}

// spans reports the hyperlink spans within one rendered row. Columns count
// display cells over the row's visible text, skipping styling and hyperlink
// sequences, so they line up with mouse coordinates.
func (s *linkScanner) spans(line string) []linkTarget {
	var spans []linkTarget
	col, start := 0, -1
	if s.pending != "" {
		start = 0
	}
	var state byte
	for i := 0; i < len(line); {
		seq, width, n, newState := ansi.DecodeSequenceWc(line[i:], state, nil)
		if n <= 0 {
			// Malformed tail: the remaining cells cannot be measured, so no
			// span is reported for it rather than one at a wrong column.
			break
		}
		state = newState
		i += n
		if width > 0 {
			col += width
			continue
		}
		if !strings.HasPrefix(seq, osc8Open) {
			continue
		}
		if destination := osc8Destination(seq); destination != "" {
			if start >= 0 {
				spans = append(spans, linkTarget{start: start, end: col, target: s.pending})
			}
			start, s.pending = col, destination
			continue
		}
		if start >= 0 {
			spans = append(spans, linkTarget{start: start, end: col, target: s.pending})
		}
		start, s.pending = -1, ""
	}
	if start >= 0 && s.pending != "" {
		// The label continues on the next row (or is the last thing rendered):
		// keep the destination pending so the wrapped cells stay clickable.
		spans = append(spans, linkTarget{start: start, end: col, target: s.pending})
	}
	return spans
}

// osc8Destination returns the URI of an OSC 8 open sequence, or "" when the
// sequence closes a hyperlink. Payload framing is "8;params;URI" followed by
// ST or BEL.
func osc8Destination(seq string) string {
	body := strings.TrimSuffix(strings.TrimSuffix(seq[len(osc8Open):], osc8Terminator), "\a")
	_, uri, found := strings.Cut(body, ";")
	if !found {
		return ""
	}
	return uri
}

// linkOpenMsg reports the outcome of launching a link destination.
type linkOpenMsg struct {
	target string
	err    error
}

// linkOpenTimeout bounds the platform opener so a launcher that never returns
// cannot leak a process per click.
const linkOpenTimeout = 5 * time.Second

// linkOpener launches destinations; it is a variable so tests can observe the
// click without starting a real application.
var linkOpener = launchLink

// openLinkCmd launches an already-validated destination with the platform
// opener. It runs off the update loop because the opener is a child process.
func (m *Model) openLinkCmd(target string) tea.Cmd {
	if strings.TrimSpace(target) == "" {
		return nil
	}
	return func() tea.Msg { return linkOpenMsg{target: target, err: linkOpener(target)} }
}

// launchLink opens a destination with the native handler. A file:// target is
// passed as a plain path: the openers do not accept the URL form.
func launchLink(target string) error {
	program, arg := "xdg-open", target
	if parsed, err := url.Parse(target); err == nil && parsed.Scheme == "file" {
		if path := parsed.Path; path != "" {
			arg = path
		}
	}
	if runtime.GOOS == "darwin" {
		program = "open"
	}
	ctx, cancel := context.WithTimeout(context.Background(), linkOpenTimeout)
	defer cancel()
	return exec.CommandContext(ctx, program, arg).Run()
}
