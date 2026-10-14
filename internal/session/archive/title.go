package archive

import (
	"strings"

	"ccdp/internal/messages"
)

// Title derives a short list title from the first user message.
func Title(history []messages.Message) string {
	for _, m := range history {
		if m.Role != messages.RoleUser || m.InternalSource != "" {
			continue
		}
		line := strings.Join(strings.Fields(m.Content), " ")
		if line == "" {
			continue
		}
		line = truncateRunes(line, 60)
		return line
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
