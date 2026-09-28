// Package changes owns immutable workspace snapshots and recoverable file plans.
// Callers supply the active path policy and own operation authorization.
package changes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ccdp/internal/atomicfile"
	"ccdp/internal/fsops"
	"ccdp/internal/sandbox"
	"ccdp/internal/workspace"
)

const chunkSize = 8 << 20

var ErrStale = errors.New("workspace changed; prepare a new plan")
var ErrConflict = errors.New("unresolved file conflicts")

type File struct {
	Kind   string   `json:"kind"`
	Mode   uint32   `json:"mode,omitempty"`
	Size   int64    `json:"size,omitempty"`
	Digest string   `json:"digest,omitempty"`
	Chunks []string `json:"chunks,omitempty"`
}

type Snapshot struct {
	ID        string          `json:"id"`
	Root      string          `json:"root"`
	CreatedAt time.Time       `json:"created_at"`
	Files     map[string]File `json:"files"`
	Excluded  []string        `json:"excluded,omitempty"`
}

type Edit struct {
	Path     string `json:"path"`
	Before   File   `json:"before"`
	After    File   `json:"after"`
	Conflict string `json:"conflict,omitempty"`
}

type Plan struct {
	ID        string    `json:"id"`
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at"`
	Edits     []Edit    `json:"edits"`
}

type Journal struct {
	Plan     Plan   `json:"plan"`
	Status   string `json:"status"`
	NextStep int    `json:"next_step"`
	Error    string `json:"error,omitempty"`
	Checksum string `json:"checksum"`
}

type Store struct {
	mu     sync.Mutex
	opMu   sync.Mutex
	dir    string
	memory map[string][]byte
	// Only publications whose file and directory syncs completed successfully
	// may be reused. Failed publications must retry the durability barrier.
	durable map[string]bool
}

// New uses memory when dir is empty. On-disk state is separate from the workspace.
func New(dir string) *Store { return &Store{dir: dir, memory: make(map[string][]byte)} }

func digest(b []byte) string   { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validID(id string) bool   { b, e := hex.DecodeString(id); return e == nil && len(b) == 32 }
func fileEqual(a, b File) bool { return a.Kind == b.Kind && a.Mode == b.Mode && a.Digest == b.Digest }
func absent() File             { return File{Kind: "absent"} }
func at(s Snapshot, p string) File {
	if f, ok := s.Files[p]; ok {
		return f
	}
	return absent()
}

func (s *Store) put(key string, data []byte) error {
	if len(data) > 64<<20 {
		return errors.New("snapshot record exceeds 64 MiB; narrow workspace scope")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		s.memory[key] = append([]byte(nil), data...)
		return nil
	}
	p := filepath.Join(s.dir, key)
	immutable := !strings.HasPrefix(key, "transactions/")
	if immutable && s.durable[key] {
		// Our immutable store owns these objects. Verify on consumption, not
		// by rereading every already-published blob on every checkpoint.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := atomicfile.WriteFile(p, data, 0600); err != nil {
		return err
	}
	if immutable {
		if s.durable == nil {
			s.durable = make(map[string]bool)
		}
		s.durable[key] = true
	}
	return nil
}

func (s *Store) get(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		b, ok := s.memory[key]
		if !ok {
			return nil, os.ErrNotExist
		}
		return append([]byte(nil), b...), nil
	}
	f, e := os.Open(filepath.Join(s.dir, key))
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 64<<20+1))
	if len(b) > 64<<20 {
		return nil, errors.New("changes record exceeds 64 MiB")
	}
	return b, e
}

func (s *Store) save(kind, id string, value any) error {
	if !validID(id) {
		return errors.New("invalid changes identity")
	}
	if kind == "transactions" {
		j, ok := value.(Journal)
		if !ok {
			return errors.New("invalid journal value")
		}
		j.Checksum = ""
		raw, e := json.Marshal(j)
		if e != nil {
			return e
		}
		j.Checksum = digest(raw)
		value = j
	}
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	return s.put(kind+"/"+id+".json", b)
}

// PutValue/Value are immutable, verified JSON artifacts used by runtime records.
func (s *Store) PutValue(value any) (string, error) {
	b, e := json.Marshal(value)
	if e != nil {
		return "", e
	}
	id := digest(b)
	return id, s.put("values/"+id, b)
}
func (s *Store) Value(id string, target any) error {
	if !validID(id) {
		return errors.New("invalid value id")
	}
	b, e := s.get("values/" + id)
	if e != nil {
		return e
	}
	if digest(b) != id {
		return errors.New("corrupt value")
	}
	return json.Unmarshal(b, target)
}

func (s *Store) bytesFile(data []byte, mode uint32, kind string) (File, error) {
	// Symlink permissions have no portable Git meaning; both capture paths use 0.
	if kind == "symlink" {
		mode = 0
	}
	f := File{Kind: kind, Mode: mode, Size: int64(len(data)), Digest: digest(data)}
	for len(data) > 0 {
		n := min(len(data), chunkSize)
		part := data[:n]
		id := f.Digest
		if f.Size > chunkSize {
			id = digest(part)
		}
		if e := s.put("blobs/"+id, part); e != nil {
			return File{}, e
		}
		f.Chunks = append(f.Chunks, id)
		data = data[n:]
	}
	return f, nil
}

func (s *Store) ImportFile(data []byte, mode uint32, kind string) (File, error) {
	return s.bytesFile(data, mode, kind)
}
func (s *Store) SaveSnapshot(v Snapshot) (Snapshot, error) {
	v.ID = ""
	v.CreatedAt = time.Now().UTC()
	b, e := json.Marshal(v)
	if e != nil {
		return v, e
	}
	v.ID = digest(b)
	return v, s.save("snapshots", v.ID, v)
}
func (s *Store) SavePlan(v Plan) (Plan, error) {
	v.ID = ""
	b, e := json.Marshal(v)
	if e != nil {
		return v, e
	}
	v.ID = digest(b)
	return v, s.save("plans", v.ID, v)
}

func (s *Store) Content(f File) ([]byte, error) {
	if f.Size < 0 || f.Size > 512<<20 {
		return nil, errors.New("file exceeds snapshot read budget (512 MiB)")
	}
	b := make([]byte, 0, int(f.Size))
	for _, id := range f.Chunks {
		if !validID(id) {
			return nil, errors.New("invalid blob id")
		}
		part, e := s.get("blobs/" + id)
		if e != nil {
			return nil, e
		}
		if digest(part) != id {
			return nil, errors.New("corrupt snapshot blob")
		}
		b = append(b, part...)
		if int64(len(b)) > f.Size {
			return nil, errors.New("corrupt snapshot size")
		}
	}
	// A single chunk's digest was already checked above.
	single := len(f.Chunks) == 1 && f.Chunks[0] == f.Digest
	if int64(len(b)) != f.Size || !single && digest(b) != f.Digest {
		return nil, errors.New("corrupt file snapshot")
	}
	return b, nil
}

func safePath(root, rel string) (string, error) {
	if !utf8.ValidString(rel) {
		return "", errors.New("non-UTF-8 paths require explicit external handling")
	}
	if rel == "" || rel == "." || filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) {
		return "", errors.New("invalid relative path")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" || part == ".ccdp" {
			return "", errors.New("control paths cannot be snapshotted")
		}
	}
	return filepath.Join(root, clean), nil
}

func (s *Store) ReadFile(root, rel string, policy *sandbox.Sandbox) (File, error) {
	p, e := safePath(root, rel)
	if e != nil {
		return File{}, e
	}
	if policy == nil {
		return File{}, errors.New("snapshot requires path policy")
	}
	if _, e = policy.ResolveRead(p); e != nil {
		return File{}, e
	}
	handle, e := os.OpenRoot(root)
	if e != nil {
		return File{}, e
	}
	defer handle.Close()
	before, e := handle.Lstat(rel)
	if os.IsNotExist(e) {
		return absent(), nil
	}
	if e != nil {
		return File{}, e
	}
	var data []byte
	kind := "regular"
	if before.Mode()&os.ModeSymlink != 0 {
		kind = "symlink"
		var target string
		target, e = handle.Readlink(rel)
		data = []byte(target)
	} else if before.Mode().IsRegular() {
		if before.Size() > 512<<20 {
			return File{}, fmt.Errorf("%s exceeds snapshot limit (512 MiB)", rel)
		}
		var f *os.File
		f, e = fsops.OpenRegularAt(handle, rel)
		if e == nil {
			data, e = io.ReadAll(io.LimitReader(f, 512<<20+1))
			e = errors.Join(e, f.Close())
		}
	} else {
		return File{}, fmt.Errorf("unsupported file type: %s", rel)
	}
	if e != nil {
		return File{}, e
	}
	after, e := handle.Lstat(rel)
	if e != nil {
		return File{}, ErrStale
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
		return File{}, ErrStale
	}
	if len(data) > 512<<20 {
		return File{}, errors.New("file exceeds snapshot limit")
	}
	return s.bytesFile(data, uint32(before.Mode().Perm()), kind)
}

// CapturePaths captures exactly the caller's admitted inventory. A nil inventory
// discovers visible non-control files; callers with Git use ls-files -z instead.
func (s *Store) CapturePaths(ctx context.Context, root string, paths []string, policy *sandbox.Sandbox) (Snapshot, error) {
	abs, e := filepath.Abs(root)
	if e != nil {
		return Snapshot{}, e
	}
	abs, e = filepath.EvalSymlinks(abs)
	if e != nil {
		return Snapshot{}, e
	}
	if paths == nil {
		paths, e = workspace.Inventory(ctx, abs, policy)
		if e != nil {
			return Snapshot{}, e
		}
	}
	out := Snapshot{Root: abs, CreatedAt: time.Now().UTC(), Files: map[string]File{}}
	sort.Strings(paths)
	for _, p := range paths {
		if e := ctx.Err(); e != nil {
			return Snapshot{}, e
		}
		f, e := s.ReadFile(abs, p, policy)
		if e != nil {
			return Snapshot{}, fmt.Errorf("capture %q: %w", p, e)
		}
		if f.Kind != "absent" {
			out.Files[p] = f
		}
	}
	b, _ := json.Marshal(out)
	out.ID = digest(b)
	if e := s.save("snapshots", out.ID, out); e != nil {
		return Snapshot{}, e
	}
	return out, nil
}

func (s *Store) LoadSnapshot(id string) (Snapshot, error) {
	var v Snapshot
	if !validID(id) {
		return v, errors.New("invalid snapshot id")
	}
	b, e := s.get("snapshots/" + id + ".json")
	if e != nil {
		return v, e
	}
	e = json.Unmarshal(b, &v)
	if e != nil {
		return v, e
	}
	saved := v.ID
	v.ID = ""
	data, _ := json.Marshal(v)
	v.ID = saved
	if saved != id || digest(data) != id {
		return Snapshot{}, errors.New("corrupt snapshot manifest")
	}
	return v, nil
}

func Paths(s Snapshot) []string {
	out := make([]string, 0, len(s.Files))
	for p := range s.Files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func Difference(base, next Snapshot) []Edit {
	paths := map[string]bool{}
	for p := range base.Files {
		paths[p] = true
	}
	for p := range next.Files {
		paths[p] = true
	}
	out := []Edit{}
	for p := range paths {
		b, n := at(base, p), at(next, p)
		if !fileEqual(b, n) {
			out = append(out, Edit{Path: p, Before: b, After: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Prepare applies base→target on top of current, retaining unrelated changes.
func (s *Store) Prepare(base, target, current Snapshot, selection []string) (Plan, error) {
	p, err := s.BuildPlan(base, target, current)
	if err != nil {
		return p, err
	}
	return s.SelectPlan(p, selection)
}

// BuildPlan computes edits and conflicts without committing an incomplete plan.
func (s *Store) BuildPlan(base, target, current Snapshot) (Plan, error) {
	p := Plan{Root: current.Root, CreatedAt: time.Now().UTC(), Edits: []Edit{}}
	difference := Difference(base, target)
	for _, edit := range difference {
		now := at(current, edit.Path)
		if fileEqual(now, edit.After) {
			continue
		}
		result := Edit{Path: edit.Path, Before: now, After: edit.After}
		if !fileEqual(now, edit.Before) {
			if now.Kind == "regular" && edit.Before.Kind == "regular" && edit.After.Kind == "regular" {
				b, e := s.Content(edit.Before)
				if e != nil {
					return p, e
				}
				o, e := s.Content(now)
				if e != nil {
					return p, e
				}
				t, e := s.Content(edit.After)
				if e != nil {
					return p, e
				}
				merged, e := MergeText(b, o, t)
				mode := now.Mode
				if edit.Before.Mode != edit.After.Mode {
					if now.Mode != edit.Before.Mode && now.Mode != edit.After.Mode {
						e = ErrConflict
					}
					mode = edit.After.Mode
				}
				if e == nil {
					result.After, e = s.bytesFile(merged, mode, "regular")
					if e != nil {
						return p, e
					}
				} else {
					result.Conflict = "overlapping content or mode changes"
				}
			} else {
				result.Conflict = "file was independently changed, removed, or replaced"
			}
		}
		p.Edits = append(p.Edits, result)
	}
	return p, nil
}

// SelectPlan validates selection against edits including unresolved conflicts.
func (s *Store) SelectPlan(p Plan, selection []string) (Plan, error) {
	if len(selection) == 0 {
		return s.SavePlan(p)
	}
	wanted := make(map[string]bool, len(selection))
	for _, path := range selection {
		wanted[path] = true
	}
	edits := make([]Edit, 0, len(selection))
	for _, edit := range p.Edits {
		if wanted[edit.Path] {
			edits = append(edits, edit)
			delete(wanted, edit.Path)
		}
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for path := range wanted {
			missing = append(missing, path)
		}
		sort.Strings(missing)
		return p, fmt.Errorf("selected paths have no source changes: %s", strings.Join(missing, ", "))
	}
	p.Edits = edits
	return s.SavePlan(p)
}

func (s *Store) LoadPlan(id string) (Plan, error) {
	var p Plan
	if !validID(id) {
		return p, errors.New("invalid plan id")
	}
	b, e := s.get("plans/" + id + ".json")
	if e != nil {
		return p, e
	}
	if e = json.Unmarshal(b, &p); e != nil {
		return p, e
	}
	p.ID = ""
	raw, _ := json.Marshal(p)
	p.ID = id
	if digest(raw) != id {
		return Plan{}, errors.New("corrupt plan")
	}
	return p, nil
}

// Materialize creates an independent copy. It never hardlinks workspace files.
func (s *Store) Materialize(ctx context.Context, snap Snapshot, dest string) error {
	root, e := os.OpenRoot(dest)
	if e != nil {
		return e
	}
	defer root.Close()
	for _, rel := range Paths(snap) {
		f := snap.Files[rel]
		b, e := s.Content(f)
		if e != nil {
			return e
		}
		if e = writeRooted(ctx, root, rel, f, b); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) Journal(id string) (Journal, error) {
	var j Journal
	if !validID(id) {
		return j, errors.New("invalid journal id")
	}
	b, e := s.get("transactions/" + id + ".json")
	if e == nil {
		e = json.Unmarshal(b, &j)
		if e == nil {
			checksum := j.Checksum
			j.Checksum = ""
			raw, _ := json.Marshal(j)
			j.Checksum = checksum
			if checksum != digest(raw) {
				e = errors.New("corrupt file transaction journal")
			}
		}
	}
	return j, e
}
