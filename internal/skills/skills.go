// Package skills implements a Claude-Code-style skill system. A skill is a
// directory with a SKILL.md file: a small frontmatter block (name +
// description) and a markdown body of instructions. Skills live in
// ~/.ccdp/skills/<name>/SKILL.md (user-scoped) and .ccdp/skills/<name>/SKILL.md
// (project-scoped).
//
// Only the frontmatter descriptions are injected into the system prompt (cheap
// to scan); the model loads the full body on demand via the ReadSkill tool.
package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

// Skill files are extension-provided input, not trusted bounded metadata.
// Limit reads before allocation so a malformed SKILL.md cannot consume an
// arbitrary amount of memory while the session is being constructed.
const (
	DefaultMaxSkillFileBytes   = 256 << 10
	DefaultMaxSkillsIndexBytes = 128 << 10
)

// Skill is one loaded skill.
type Skill struct {
	Name        string
	Description string
	Body        string
	Path        string // directory containing SKILL.md
	Source      string // "user" or "project"
}

// Store holds the merged set of user + project skills.
type Store struct {
	mu     sync.RWMutex
	skills []Skill
	byName map[string]Skill
}

// Provider is the narrow interface tools need to look up skills.
type Provider interface {
	Get(name string) (Skill, bool)
	All() []Skill
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{byName: map[string]Skill{}}
}

// Load scans userDir and each project dir for skills and populates the store.
// Project skills override user skills; within projects, earlier dirs win.
func (s *Store) Load(userDir string, projectDirs ...string) {
	_ = s.load(userDir, false, projectDirs...)
}

// LoadChecked is the error-returning form of Load. Missing SKILL.md files are
// optional during directory discovery and remain ignored; a present special
// file, oversized body, or other read failure is returned so a caller that
// selected or trusted that input can surface the problem instead of silently
// dropping it. The store is swapped only after a complete successful scan.
func (s *Store) LoadChecked(userDir string, projectDirs ...string) error {
	return s.load(userDir, true, projectDirs...)
}

func (s *Store) load(userDir string, strict bool, projectDirs ...string) error {
	dirs := make([]string, 0, len(projectDirs)+1)
	dirs = append(dirs, userDir)
	dirs = append(dirs, projectDirs...)

	seen := map[string]bool{}
	loadedSkills := make([]Skill, 0)
	loadedByName := make(map[string]Skill)

	// De-duplication is first-come-first-served, so process earlier project
	// dirs first (the first project dir wins the slot) and the user dir last
	// (project skills always override user skills).
	order := make([]string, 0, len(dirs))
	order = append(order, dirs[1:]...)
	order = append(order, dirs[0]) // user dir last

	for _, dir := range order {
		if dir == "" {
			continue
		}
		dirInfo, err := os.Stat(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if !strict {
				continue
			}
			return fmt.Errorf("skills: stat directory %q: %w", dir, err)
		}
		if !dirInfo.IsDir() {
			if !strict {
				continue
			}
			return fmt.Errorf("skills: %q is not a directory", dir)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if !strict {
				continue
			}
			return fmt.Errorf("skills: read directory %q: %w", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dirName := e.Name()
			md := filepath.Join(dir, dirName, "SKILL.md")
			data, truncated, err := readBounded(md, DefaultMaxSkillFileBytes)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if !strict {
					continue
				}
				return fmt.Errorf("skills: read %q: %w", md, err)
			}
			if truncated && strict {
				return fmt.Errorf("skills: %q exceeds %d bytes", md, DefaultMaxSkillFileBytes)
			}
			content := string(data)
			if truncated {
				content += fmt.Sprintf("\n\n…[skill truncated at %d bytes]", DefaultMaxSkillFileBytes)
			}
			sk := parse(content, dirName, filepath.Join(dir, dirName), dir == userDir)
			if sk.Name == "" || sk.Body == "" {
				continue
			}
			// De-duplicate by the parsed skill name so project skills win.
			if seen[sk.Name] {
				continue
			}
			seen[sk.Name] = true
			loadedSkills = append(loadedSkills, sk)
			loadedByName[sk.Name] = sk
		}
	}
	sort.Slice(loadedSkills, func(i, j int) bool { return loadedSkills[i].Name < loadedSkills[j].Name })
	s.mu.Lock()
	s.skills = loadedSkills
	s.byName = loadedByName
	s.mu.Unlock()
	return nil
}

// All returns the loaded skills sorted by name.
func (s *Store) All() []Skill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Skill, len(s.skills))
	copy(out, s.skills)
	return out
}

// Get returns a skill by name.
func (s *Store) Get(name string) (Skill, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sk, ok := s.byName[name]
	return sk, ok
}

// SkillsSection renders the skill index for the system prompt.
func (s *Store) SkillsSection() string {
	return s.SkillsSectionBounded(DefaultMaxSkillsIndexBytes)
}

// SkillsSectionBounded renders the cheap skill index with a hard byte cap.
// Skill bodies are loaded into the store with DefaultMaxSkillFileBytes; only
// names/descriptions are included here, while ReadSkill applies its own tool
// output bound when a body is requested.
func (s *Store) SkillsSectionBounded(maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	all := s.All()
	if len(all) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nAvailable skills (call ReadSkill with the exact name to load one):\n")
	if sb.Len() >= maxBytes {
		return truncateUTF8(sb.String(), maxBytes)
	}
	for _, sk := range all {
		desc := strings.ReplaceAll(sk.Description, "\n", " ")
		line := fmt.Sprintf("  - %s: %s\n", sk.Name, desc)
		remaining := maxBytes - sb.Len()
		if len(line) > remaining {
			if remaining > 0 {
				sb.WriteString(truncateUTF8(line, remaining))
			}
			break
		}
		sb.WriteString(line)
	}
	return sb.String()
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func readBounded(path string, maxBytes int) ([]byte, bool, error) {
	if maxBytes <= 0 {
		return nil, false, fmt.Errorf("skills: invalid read limit")
	}
	// A skill is a file-backed input, not a stream.  Check before opening so a
	// FIFO cannot block session construction, then check the opened descriptor
	// as well in case the path was replaced between the checks.
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("skills: %s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, false, fmt.Errorf("skills: %s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}

// parse splits frontmatter and body from a SKILL.md file.
func parse(content, fallbackName, path string, user bool) Skill {
	body, fm := splitFrontmatter(content)
	name := fm["name"]
	if name == "" {
		name = fallbackName
	}
	desc := fm["description"]
	sk := Skill{
		Name:        name,
		Description: desc,
		Body:        strings.TrimSpace(body),
		Path:        path,
	}
	if user {
		sk.Source = "user"
	} else {
		sk.Source = "project"
	}
	return sk
}

// splitFrontmatter extracts a leading --- delimited key: value block.
func splitFrontmatter(content string) (body string, kv map[string]string) {
	kv = map[string]string{}
	trimmed := strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(trimmed, "---") {
		return trimmed, kv
	}
	rest := strings.TrimPrefix(trimmed, "---")
	rest = strings.TrimPrefix(rest, "\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return trimmed, kv
	}
	fm := rest[:end]
	body = strings.TrimSpace(rest[end+4:])
	lines := strings.Split(fm, "\n")
	var key, val string
	flush := func() {
		if key != "" {
			kv[key] = strings.TrimSpace(strings.Trim(val, `"'`))
		}
		key, val = "", ""
	}
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if i := strings.Index(ln, ":"); i > 0 {
			flush()
			key = strings.TrimSpace(ln[:i])
			val = strings.TrimSpace(ln[i+1:])
			continue
		}
		// Continuation line of a folded description.
		if key != "" && val != "" {
			val += " " + ln
		}
	}
	flush()
	return body, kv
}
