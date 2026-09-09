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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
// Later sources win on name collision (project over user, later dirs first).
func (s *Store) Load(userDir string, projectDirs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Project skills override user skills; within projects, earlier dirs win.
	dirs := make([]string, 0, len(projectDirs)+1)
	dirs = append(dirs, userDir)
	dirs = append(dirs, projectDirs...)

	seen := map[string]bool{}
	s.skills = s.skills[:0]
	s.byName = map[string]Skill{}

	// Apply project dirs in reverse so the first project dir wins the slot,
	// but user dir is always lowest priority.
	order := make([]string, 0, len(dirs))
	for i := len(dirs) - 1; i >= 1; i-- {
		order = append(order, dirs[i])
	}
	order = append(order, dirs[0]) // user dir last

	for _, dir := range order {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dirName := e.Name()
			md := filepath.Join(dir, dirName, "SKILL.md")
			data, err := os.ReadFile(md)
			if err != nil {
				continue
			}
			sk := parse(string(data), dirName, filepath.Join(dir, dirName), dir == userDir)
			if sk.Name == "" || sk.Body == "" {
				continue
			}
			// De-duplicate by the parsed skill name so project skills win.
			if seen[sk.Name] {
				continue
			}
			seen[sk.Name] = true
			s.skills = append(s.skills, sk)
			s.byName[sk.Name] = sk
		}
	}
	sort.Slice(s.skills, func(i, j int) bool { return s.skills[i].Name < s.skills[j].Name })
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
	all := s.All()
	if len(all) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nAvailable skills (call ReadSkill with the exact name to load one):\n")
	for _, sk := range all {
		desc := strings.ReplaceAll(sk.Description, "\n", " ")
		fmt.Fprintf(&sb, "  - %s: %s\n", sk.Name, desc)
	}
	return sb.String()
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
