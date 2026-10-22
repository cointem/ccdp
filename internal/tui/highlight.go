package tui

import (
	"bytes"
	"container/list"
	"os"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var (
	highlightMu         sync.Mutex
	highlightCache      = make(map[highlightKey]*list.Element)
	highlightLRU        = list.New()
	highlightCacheBytes int
)

const maxHighlightCacheSize = 256
const maxHighlightCacheBytes = 8 << 20
const maxHighlightEntryBytes = 256 << 10

type highlightCacheEntry struct {
	key   highlightKey
	lines []string
	bytes int
}

type highlightKey struct {
	source, language, style string
}

// highlightCode runs chroma syntax highlighting on source code lines.
// It returns a slice of styled lines without trailing newlines.
// If language is empty or unrecognized, or color is disabled, or an error occurs,
// it returns nil so the caller can fall back to standard styling.
func highlightCode(lines []string, language string, dark bool) []string {
	if len(lines) == 0 {
		return nil
	}
	lang := strings.TrimSpace(language)
	if lang == "" || isPlaintextLanguage(lang) {
		return nil
	}
	if isColorDisabled() {
		return nil
	}

	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Match("dummy." + lang)
	}
	if lexer == nil {
		return nil
	}
	lexer = chroma.Coalesce(lexer)

	styleName := "github"
	if dark {
		styleName = "monokai"
	}
	style := styles.Get(styleName)
	if style == nil {
		style = styles.Fallback
	}

	source := strings.Join(lines, "\n")
	cacheKey := highlightKey{source: source, language: lang, style: styleName}

	highlightMu.Lock()
	if elem, ok := highlightCache[cacheKey]; ok {
		highlightLRU.MoveToFront(elem)
		res := elem.Value.(*highlightCacheEntry).lines
		highlightMu.Unlock()
		out := make([]string, len(res))
		copy(out, res)
		return out
	}
	highlightMu.Unlock()

	iterator, err := lexer.Tokenise(nil, source)
	if err != nil {
		return nil
	}

	formatter := formatters.TTY256
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, iterator); err != nil {
		return nil
	}

	formatted := buf.String()
	formatted = strings.TrimSuffix(formatted, "\n")
	resLines := strings.Split(formatted, "\n")

	if len(resLines) != len(lines) {
		return nil
	}

	size := len(source) + len(lang) + len(styleName) + len(formatted) + 16*len(resLines)
	// Large blocks can be rendered without retaining both source and ANSI text.
	if size > maxHighlightEntryBytes {
		return resLines
	}
	highlightMu.Lock()
	if elem, ok := highlightCache[cacheKey]; ok {
		highlightLRU.MoveToFront(elem)
		// A competing render already published this immutable entry.
	} else {
		for highlightLRU.Len() >= maxHighlightCacheSize || highlightCacheBytes+size > maxHighlightCacheBytes {
			oldest := highlightLRU.Back()
			if oldest != nil {
				highlightLRU.Remove(oldest)
				entry := oldest.Value.(*highlightCacheEntry)
				delete(highlightCache, entry.key)
				highlightCacheBytes -= entry.bytes
			}
		}
		elem := highlightLRU.PushFront(&highlightCacheEntry{
			key:   cacheKey,
			lines: resLines,
			bytes: size,
		})
		highlightCache[cacheKey] = elem
		highlightCacheBytes += size
	}
	highlightMu.Unlock()

	out := make([]string, len(resLines))
	copy(out, resLines)
	return out
}

func isPlaintextLanguage(lang string) bool {
	l := strings.ToLower(lang)
	switch l {
	case "text", "txt", "plain", "plaintext", "output", "none", "raw":
		return true
	}
	return false
}

func isColorDisabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return true
	}
	p := lipgloss.ColorProfile()
	// termenv.Ascii is 3. Only disable when explicitly Ascii/monochrome AND term is not default non-tty testing
	// Note: lipgloss.HasDarkBackground() or DefaultRenderer().ColorProfile() handles actual profile.
	// In non-tty testing without TERM, profile is Ascii (3). If NO_COLOR is empty, we allow highlighting.
	if p == termenv.Ascii && os.Getenv("TERM") == "dumb" {
		return true
	}
	return false
}
