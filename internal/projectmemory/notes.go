package projectmemory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"ccdp/internal/fsops"
)

// Frontmatter is JSON (also valid YAML), so quoting, multiline text and source
// metadata round-trip without a second serialization language. The body is
// ordinary editable Markdown. MEMORY.md is a disposable generated index.
func encodeEntry(e Entry) ([]byte, error) {
	body := e.Body
	e.Body = ""
	if e.ExcludedSources == nil {
		e.ExcludedSources = []Source{}
	}
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return nil, err
	}
	return []byte("---\n" + string(raw) + "\n---\n\n" + body + "\n"), nil
}

func readEntry(root *os.Root, id string) (Entry, error) {
	if !validID.MatchString(id) {
		return Entry{}, errors.New("invalid memory id")
	}
	f, err := fsops.OpenRegularAt(root, id+".md")
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxEntryBytes+1))
	if err != nil {
		return Entry{}, err
	}
	if len(raw) > MaxEntryBytes || !utf8.Valid(raw) {
		return Entry{}, fmt.Errorf("invalid or oversized memory %s", id)
	}
	e := Entry{ID: id, Kind: "project"}
	body := string(raw)
	if strings.HasPrefix(body, "---\n") {
		meta, rest, found := strings.Cut(body[4:], "\n---\n")
		if !found {
			return Entry{}, fmt.Errorf("invalid memory frontmatter: %s", id)
		}
		if err := json.Unmarshal([]byte(meta), &e); err != nil {
			return Entry{}, fmt.Errorf("read memory %s: %w", id, err)
		}
		body = rest
	}
	if e.ID != id {
		return Entry{}, fmt.Errorf("memory id does not match filename: %s", id)
	}
	e.Body = strings.TrimSpace(body)
	if e.ExcludedSources == nil {
		// Older files did not distinguish ordinary saves from corrections.
		// Preserve their conservative exclusion when an entry was modified.
		if !e.CreatedAt.IsZero() && e.UpdatedAt.After(e.CreatedAt) {
			excludeSources(&e)
		}
		if e.ExcludedSources == nil {
			e.ExcludedSources = []Source{}
		}
	}
	if e.Deleted {
		return e, nil
	}
	if err := normalize(&e); err != nil {
		return Entry{}, fmt.Errorf("memory %s: %w", id, err)
	}
	return e, nil
}

func list(root *os.Root, includeDeleted bool) ([]Entry, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	entries := []Entry{}
	total := 0
	for _, name := range names {
		if name == "MEMORY.md" || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		e, err := readEntry(root, strings.TrimSuffix(name, ".md"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if e.Deleted && !includeDeleted {
			continue
		}
		entries = append(entries, e)
		if !e.Deleted {
			raw, _ := encodeEntry(e)
			total += len(raw)
		}
		if total > MaxTotalBytes {
			return nil, errors.New("memory exceeds storage limit; remove or shorten notes")
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

func (s *Store) List() ([]Entry, error) {
	root, err := s.openRoot(false)
	if os.IsNotExist(err) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return list(root, false)
}

func (s *Store) Read(id string) (Entry, error) {
	root, err := s.openRoot(false)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	e, err := readEntry(root, id)
	if err == nil && e.Deleted {
		err = os.ErrNotExist
	}
	return e, err
}

func brief(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

func normalize(e *Entry) error {
	e.Body, e.Title = strings.TrimSpace(e.Body), strings.TrimSpace(e.Title)
	if e.Kind == "" {
		e.Kind = "project"
	}
	if !slices.Contains([]string{"user", "feedback", "project", "reference"}, e.Kind) {
		return errors.New("invalid memory kind")
	}
	if !validID.MatchString(e.ID) || e.ID == "memory" {
		return errors.New("invalid or reserved memory id")
	}
	if e.Body == "" || !utf8.ValidString(e.Body+e.Title) || len(e.Title) > 320 {
		return errors.New("memory requires nonempty UTF-8 content and a title up to 320 bytes")
	}
	if e.Title == "" {
		e.Title = inferredTitle(e.Body)
	}
	// Always derive the preview from the current body, including external edits.
	e.Summary = brief(e.Body, 100)
	return nil
}

func addSource(e *Entry, source Entry) {
	e.SourceSession, e.SourceTurn, e.SourceCommand = source.SourceSession, source.SourceTurn, source.SourceCommand
	src := Source{source.SourceSession, source.SourceTurn, source.SourceCommand}
	if src.Session != "" && !slices.Contains(e.Sources, src) {
		e.Sources = append(e.Sources, src)
	}
}

func excludeSources(e *Entry) {
	for _, src := range e.Sources {
		if !slices.Contains(e.ExcludedSources, src) {
			e.ExcludedSources = append(e.ExcludedSources, src)
		}
	}
}

// Update creates a note or replaces the exact body previously read. Plain text
// comparison protects stale writers without hashes or revision counters.
// Empty previous means create, never an unconditional overwrite.
func (s *Store) Update(ctx context.Context, entry Entry, previous string) (Entry, error) {
	return s.mutate(ctx, func(root *os.Root) (Entry, error) {
		if entry.ID == "" {
			id, err := newNoteID()
			if err != nil {
				return Entry{}, err
			}
			entry.ID = id
		}
		if err := normalize(&entry); err != nil {
			return Entry{}, err
		}
		old, err := readEntry(root, entry.ID)
		if err != nil && !os.IsNotExist(err) {
			return Entry{}, err
		}
		if err == nil && !old.Deleted {
			if previous == "" {
				if old.Body == entry.Body && old.Kind == entry.Kind {
					addSource(&old, entry)
					return old, nil
				}
				return Entry{}, ErrConflict
			}
			if old.Body != previous {
				return Entry{}, ErrConflict
			}
			entry.CreatedAt, entry.Sources = old.CreatedAt, old.Sources
			entry.ExcludedSources = old.ExcludedSources
			if entry.Body != old.Body || entry.Kind != old.Kind {
				excludeSources(&entry)
			}
		} else if previous != "" {
			return Entry{}, ErrConflict
		} else if err == nil && old.Deleted {
			// Reusing an ID must not erase the old note's forgetting markers.
			entry.Sources, entry.ExcludedSources = old.Sources, old.ExcludedSources
			excludeSources(&entry)
		}
		if previous == "" {
			entries, err := list(root, false)
			if err != nil {
				return Entry{}, err
			}
			for _, existing := range entries {
				if existing.Body == entry.Body && existing.Kind == entry.Kind {
					addSource(&existing, entry)
					return existing, nil
				}
			}
		}
		entry.Deleted = false
		addSource(&entry, entry)
		return entry, nil
	})
}

// Edit applies one exact text replacement to the latest body while locked.
// Independent edits to different passages survive concurrent sessions.
func (s *Store) Edit(ctx context.Context, id, oldText, newText string, source Entry) (Entry, error) {
	return s.mutate(ctx, func(root *os.Root) (Entry, error) {
		e, err := readEntry(root, id)
		if err != nil {
			return Entry{}, err
		}
		if e.Deleted {
			return Entry{}, os.ErrNotExist
		}
		if oldText == "" || strings.Count(e.Body, oldText) != 1 {
			return Entry{}, ErrConflict
		}
		autoTitle := e.Title == inferredTitle(e.Body)
		if oldText != newText {
			excludeSources(&e)
		}
		e.Body = strings.Replace(e.Body, oldText, newText, 1)
		if autoTitle {
			e.Title = ""
		}
		addSource(&e, source)
		if err := normalize(&e); err != nil {
			return Entry{}, err
		}
		return e, nil
	})
}

func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.mutate(ctx, func(root *os.Root) (Entry, error) {
		e, err := readEntry(root, id)
		if err != nil {
			return Entry{}, err
		}
		// Keep only identity and source references to exclude old evidence from
		// optional history extraction. Forgotten content is removed.
		e.Deleted = true
		e.Title, e.Summary, e.Body = "", "", ""
		return e, nil
	})
	return err
}

func (s *Store) mutate(ctx context.Context, fn func(*os.Root) (Entry, error)) (Entry, error) {
	if s.readOnly {
		return Entry{}, ErrReadOnly
	}
	root, err := s.openRoot(true)
	if err != nil {
		return Entry{}, err
	}
	defer root.Close()
	lock, err := acquire(ctx, root)
	if err != nil {
		return Entry{}, err
	}
	defer lock.Close()
	entry, err := fn(root)
	if err != nil {
		return Entry{}, err
	}
	entry.UpdatedAt = time.Now().UTC()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = entry.UpdatedAt
	}
	raw, err := encodeEntry(entry)
	if err != nil {
		return Entry{}, err
	}
	if len(raw) > MaxEntryBytes {
		return Entry{}, errors.New("memory entry exceeds size limit")
	}
	// Deletion must remain available to recover an overfull or partly damaged
	// directory; unrelated notes need not be readable to forget this one.
	if !entry.Deleted {
		entries, err := list(root, false)
		if err != nil {
			return Entry{}, err
		}
		total, count := len(raw), 1
		for _, e := range entries {
			if e.ID == entry.ID {
				continue
			}
			b, _ := encodeEntry(e)
			total += len(b)
			count++
		}
		if total > MaxTotalBytes || count > MaxEntries {
			return Entry{}, errors.New("memory is full; merge or remove obsolete notes")
		}
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if err := write(root, entry.ID+".md", raw); err != nil {
		return Entry{}, err
	}
	// The note is authoritative. A failed index refresh cannot undo a saved
	// note; reads rebuild the index, so stale cached summaries are never injected.
	_ = refreshIndex(root)
	return entry, nil
}

func renderIndex(entries []Entry) string {
	var out strings.Builder
	out.WriteString("# Memory\n\n")
	for i, e := range entries {
		line := fmt.Sprintf("- [%s](%s.md) (%s): %s\n", brief(e.Title, 60), e.ID, e.Kind, e.Summary)
		if out.Len()+len(line)+100 > MaxIndexBytes {
			fmt.Fprintf(&out, "%d more notes; use ReadProjectMemory to search or list them.\n", len(entries)-i)
			break
		}
		out.WriteString(line)
	}
	return out.String()
}

func refreshIndex(root *os.Root) error {
	entries, err := list(root, false)
	if err != nil {
		return err
	}
	return write(root, "MEMORY.md", []byte(renderIndex(entries)))
}

func (s *Store) Index() (string, error) {
	entries, err := s.List()
	if err != nil || len(entries) == 0 {
		return "", err
	}
	return renderIndex(entries), nil
}

// Tidy merges exact duplicates and rebuilds the derived index. Semantic merges
// remain deliberate model edits with original-text checks, never blind rewrites.
func (s *Store) Tidy(ctx context.Context) (int, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	root, err := s.openRoot(true)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	lock, err := acquire(ctx, root)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	entries, err := list(root, false)
	if err != nil {
		return 0, err
	}
	seen := map[string]Entry{}
	count := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		key := e.Kind + "\n" + e.Body
		prior, duplicate := seen[key]
		if !duplicate {
			seen[key] = e
			continue
		}
		for _, src := range e.Sources {
			if !slices.Contains(prior.Sources, src) {
				prior.Sources = append(prior.Sources, src)
			}
		}
		for _, src := range e.ExcludedSources {
			if !slices.Contains(prior.ExcludedSources, src) {
				prior.ExcludedSources = append(prior.ExcludedSources, src)
			}
		}
		raw, _ := encodeEntry(prior)
		if len(raw) > MaxEntryBytes {
			continue
		}
		if err := write(root, prior.ID+".md", raw); err != nil {
			return count, err
		}
		e.Deleted, e.Title, e.Body, e.Summary = true, "", "", ""
		// Evidence was transferred to the surviving note. Deduplication is not
		// a forget request and must not exclude the entire source conversation.
		e.Sources = nil
		raw, _ = encodeEntry(e)
		if err := write(root, e.ID+".md", raw); err != nil {
			return count, err
		}
		seen[key] = prior
		count++
	}
	return count, refreshIndex(root)
}

func newNoteID() (string, error) {
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return "note-" + hex.EncodeToString(nonce[:]), nil
}

func inferredTitle(body string) string {
	return brief(strings.TrimLeft(strings.SplitN(body, "\n", 2)[0], "# "), 60)
}
