package workspace

import (
	"sort"
	"strings"
	"unicode"
)

// RelevantRepoMap keeps entry points and query-related paths within the prompt
// budget, then spreads remaining entries across directories.
func (i *Info) RelevantRepoMap(limit int, query string) string {
	if i == nil {
		return ""
	}
	if limit <= 0 {
		limit = 120
	}
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })
	type ranked struct {
		entry Entry
		score int
	}
	rows := []ranked{}
	for _, entry := range i.Files {
		if entry.IsDir {
			continue
		}
		path := strings.ToLower(entry.RelPath)
		score := 0
		for _, word := range words {
			if len(word) >= 3 && strings.Contains(path, word) {
				score += 20
			}
		}
		for _, name := range []string{"readme", "agents.md", "go.mod", "package.json", "cargo.toml", "pyproject.toml", "main.", "app.", "index."} {
			if strings.Contains(path, name) {
				score += 5
			}
		}
		score -= strings.Count(path, "/")
		rows = append(rows, ranked{entry, score})
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].score != rows[b].score {
			return rows[a].score > rows[b].score
		}
		return rows[a].entry.RelPath < rows[b].entry.RelPath
	})
	out := *i
	out.Files = nil
	dirs := map[string]int{}
	deferred := []Entry{}
	for _, row := range rows {
		parts := strings.Split(row.entry.RelPath, "/")
		dir := parts[0]
		if len(parts) == 1 {
			dir = "."
		}
		if row.score <= 0 && dirs[dir] >= max(2, limit/8) {
			deferred = append(deferred, row.entry)
			continue
		}
		dirs[dir]++
		out.Files = append(out.Files, row.entry)
		if len(out.Files) == limit {
			break
		}
	}
	for _, entry := range deferred {
		if len(out.Files) == limit {
			break
		}
		out.Files = append(out.Files, entry)
	}
	sort.Slice(out.Files, func(a, b int) bool { return out.Files[a].RelPath < out.Files[b].RelPath })
	text := out.RepoMap(limit)
	if len(rows) > len(out.Files) {
		text += "\nRepository map is a relevant sample. Use Glob/LS to find paths and Grep to search file contents.\n"
	}
	return text
}
