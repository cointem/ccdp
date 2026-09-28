package changes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type Hunk struct {
	ID           string   `json:"id"`
	Path         string   `json:"path"`
	Start        int      `json:"start"`
	End          int      `json:"end"`
	Replacement  []string `json:"replacement"`
	BeforeDigest string   `json:"before_digest"`
	AfterDigest  string   `json:"after_digest"`
}

func (s *Store) Hunks(base, target Snapshot) ([]Hunk, error) {
	out := []Hunk{}
	for _, change := range Difference(base, target) {
		if change.Before.Kind != "regular" || change.After.Kind != "regular" {
			continue
		}
		b, e := s.Content(change.Before)
		if e != nil {
			return nil, e
		}
		n, e := s.Content(change.After)
		if e != nil {
			return nil, e
		}
		if bytes.IndexByte(b, 0) >= 0 || bytes.IndexByte(n, 0) >= 0 {
			continue
		}
		edits, e := lineEdits(strings.SplitAfter(string(b), "\n"), strings.SplitAfter(string(n), "\n"))
		if e != nil {
			continue
		}
		for _, v := range edits {
			h := Hunk{Path: change.Path, Start: v.start, End: v.end, Replacement: v.lines, BeforeDigest: change.Before.Digest, AfterDigest: change.After.Digest}
			data, _ := json.Marshal(h)
			h.ID = digest(data)
			out = append(out, h)
		}
	}
	return out, nil
}

func (s *Store) Select(base, target Snapshot, paths, hunkIDs []string) (Snapshot, error) {
	out := Snapshot{Root: target.Root, Files: map[string]File{}}
	for p, f := range base.Files {
		out.Files[p] = f
	}
	selected := map[string]bool{}
	for _, p := range paths {
		selected[p] = true
	}
	found := map[string]bool{}
	for _, v := range Difference(base, target) {
		if selected[v.Path] {
			found[v.Path] = true
			if v.After.Kind == "absent" {
				delete(out.Files, v.Path)
			} else {
				out.Files[v.Path] = v.After
			}
		}
	}
	for p := range selected {
		if !found[p] {
			return out, fmt.Errorf("selected path is unchanged or unknown: %s", p)
		}
	}
	hunks, e := s.Hunks(base, target)
	if e != nil {
		return out, e
	}
	wanted := map[string]bool{}
	for _, id := range hunkIDs {
		wanted[id] = true
	}
	byPath := map[string][]Hunk{}
	for _, h := range hunks {
		if wanted[h.ID] {
			if selected[h.Path] {
				return out, fmt.Errorf("select a file or its hunks, not both: %s", h.Path)
			}
			byPath[h.Path] = append(byPath[h.Path], h)
			delete(wanted, h.ID)
		}
	}
	if len(wanted) > 0 {
		return out, fmt.Errorf("unknown or stale hunk id")
	}
	for path, hs := range byPath {
		f := base.Files[path]
		b, e := s.Content(f)
		if e != nil {
			return out, e
		}
		lines := strings.SplitAfter(string(b), "\n")
		var result strings.Builder
		pos := 0
		for _, h := range hs {
			result.WriteString(strings.Join(lines[pos:h.Start], ""))
			result.WriteString(strings.Join(h.Replacement, ""))
			pos = h.End
		}
		result.WriteString(strings.Join(lines[pos:], ""))
		f, e = s.ImportFile([]byte(result.String()), f.Mode, "regular")
		if e != nil {
			return out, e
		}
		out.Files[path] = f
	}
	return s.SaveSnapshot(out)
}

// Resolve produces a new immutable preview; the caller must apply its new ID.
// "current" keeps the observed file; "target" explicitly accepts the proposed
// version. Unmentioned conflicts remain blocking.
func (s *Store) Resolve(p Plan, choices map[string]string) (Plan, error) {
	used := map[string]bool{}
	edits := []Edit{}
	for _, edit := range p.Edits {
		choice, ok := choices[edit.Path]
		if ok {
			used[edit.Path] = true
			if edit.Conflict == "" {
				return Plan{}, fmt.Errorf("%s has no conflict", edit.Path)
			}
			switch choice {
			case "current":
				continue
			case "target":
				edit.Conflict = ""
			default:
				return Plan{}, fmt.Errorf("invalid resolution for %s", edit.Path)
			}
		}
		edits = append(edits, edit)
	}
	for path := range choices {
		if !used[path] {
			return Plan{}, fmt.Errorf("unknown conflict %s", path)
		}
	}
	p.Edits = edits
	return s.SavePlan(p)
}
