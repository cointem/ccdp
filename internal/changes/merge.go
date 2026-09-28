package changes

import (
	"bytes"
	"strings"
)

type textEdit struct {
	start, end int
	lines      []string
}

// lineEdits is bounded LCS: beyond the budget a caller reports a conflict
// instead of consuming quadratic memory on generated or minified files.
func lineEdits(base, next []string) ([]textEdit, error) {
	if (len(base)+1)*(len(next)+1) > 2_000_000 {
		return nil, ErrConflict
	}
	w := len(next) + 1
	dp := make([]int, (len(base)+1)*w)
	for i := len(base) - 1; i >= 0; i-- {
		for j := len(next) - 1; j >= 0; j-- {
			if base[i] == next[j] {
				dp[i*w+j] = dp[(i+1)*w+j+1] + 1
			} else {
				dp[i*w+j] = max(dp[(i+1)*w+j], dp[i*w+j+1])
			}
		}
	}
	var out []textEdit
	i, j := 0, 0
	for i < len(base) || j < len(next) {
		if i < len(base) && j < len(next) && base[i] == next[j] {
			i++
			j++
			continue
		}
		e := textEdit{start: i}
		for i < len(base) || j < len(next) {
			if i < len(base) && j < len(next) && base[i] == next[j] {
				break
			}
			if j < len(next) && (i == len(base) || dp[i*w+j+1] >= dp[(i+1)*w+j]) {
				e.lines = append(e.lines, next[j])
				j++
			} else {
				i++
			}
		}
		e.end = i
		out = append(out, e)
	}
	return out, nil
}

// MergeText combines independent edits, preserving exact line endings.
func MergeText(base, ours, theirs []byte) ([]byte, error) {
	if bytes.Equal(ours, theirs) || bytes.Equal(base, theirs) {
		return ours, nil
	}
	if bytes.Equal(base, ours) {
		return theirs, nil
	}
	if bytes.IndexByte(base, 0) >= 0 || bytes.IndexByte(ours, 0) >= 0 || bytes.IndexByte(theirs, 0) >= 0 {
		return nil, ErrConflict
	}
	b := strings.SplitAfter(string(base), "\n")
	a, e := lineEdits(b, strings.SplitAfter(string(ours), "\n"))
	if e != nil {
		return nil, e
	}
	c, e := lineEdits(b, strings.SplitAfter(string(theirs), "\n"))
	if e != nil {
		return nil, e
	}
	var edits []textEdit
	i, j := 0, 0
	for i < len(a) || j < len(c) {
		if i == len(a) {
			edits = append(edits, c[j:]...)
			break
		}
		if j == len(c) {
			edits = append(edits, a[i:]...)
			break
		}
		x, y := a[i], c[j]
		if x.start == y.start && x.end == y.end && strings.Join(x.lines, "") == strings.Join(y.lines, "") {
			edits = append(edits, x)
			i++
			j++
			continue
		}
		if x.end <= y.start && x.start != y.start {
			edits = append(edits, x)
			i++
			continue
		}
		if y.end <= x.start && x.start != y.start {
			edits = append(edits, y)
			j++
			continue
		}
		return nil, ErrConflict
	}
	var out strings.Builder
	pos := 0
	for _, e := range edits {
		out.WriteString(strings.Join(b[pos:e.start], ""))
		out.WriteString(strings.Join(e.lines, ""))
		pos = e.end
	}
	out.WriteString(strings.Join(b[pos:], ""))
	return []byte(out.String()), nil
}
