// Package sandbox builds the fixed macOS Seatbelt policy and the application
// path policy used by built-in file tools. Permission approval admits an
// operation; this package limits the resources that operation can reach.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const BackendPath = "/usr/bin/sandbox-exec"

// Sandbox permits host reads but confines writes to authorized roots. Snapshot returns a detached policy for
// one invocation so later grants or revocations cannot mutate its profile.
type Sandbox struct {
	Workspace string
	Revision  uint64

	AdditionalDirs   []string
	ReadOnlyDirs     []string
	DisallowedDirs   []string
	ProtectedDirs    []string
	ProtectedEntries []string
	ScratchDirs      []string
	// ExecutionReadRoots are narrow, invocation-specific install roots needed
	// to load the selected executable and its bundled runtime files. They are
	// derived by execution and are not persistent user directory grants.
	ExecutionReadRoots []string
	// ExecutionScratchDirs are per-invocation temporary roots. They are not
	// durable path grants and do not change the persisted policy revision.
	ExecutionScratchDirs []string
	Limits               *Limits
	AllowNetwork         bool
	LocalServices        []LocalService
	executionGuard       *ExecutionGuard

	mu sync.RWMutex
}

// ExecutionGuard records that a model-reachable local process has run under
// this session's Seatbelt policy. Seatbelt follows descendants across fork and
// setsid, while the harness cannot prove that all such descendants have exited.
// A sticky record lets revoke/tighten operations fail closed instead of
// mistaking the leader's Wait result for proof that the old policy is gone.
type ExecutionGuard struct {
	mu                sync.Mutex
	externalExecution bool
}

func (g *ExecutionGuard) MarkExternalExecution() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.externalExecution = true
	g.mu.Unlock()
}

func (g *ExecutionGuard) ExternalExecutionPossible() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.externalExecution
}

// MarkExternalExecution records that an execution path launched an arbitrary
// model-reachable local process using this policy.
func (s *Sandbox) MarkExternalExecution() {
	if s == nil {
		return
	}
	s.mu.RLock()
	guard := s.executionGuard
	s.mu.RUnlock()
	guard.MarkExternalExecution()
}

// ExternalExecutionPossible reports whether a local process has run under this
// policy and may still have descendants retaining its Seatbelt authorization.
func (s *Sandbox) ExternalExecutionPossible() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	guard := s.executionGuard
	s.mu.RUnlock()
	return guard.ExternalExecutionPossible()
}

// ExecutionGuard returns the session-shared witness used when rebuilding
// policies or creating a child session.
func (s *Sandbox) ExecutionGuard() *ExecutionGuard {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	guard := s.executionGuard
	s.mu.RUnlock()
	return guard
}

// SetExecutionGuard attaches a session-shared witness. A nil guard is ignored
// so a sandbox can never silently lose its execution history.
func (s *Sandbox) SetExecutionGuard(guard *ExecutionGuard) {
	if s == nil || guard == nil {
		return
	}
	s.mu.Lock()
	s.executionGuard = guard
	s.mu.Unlock()
}

// ShareExecutionGuard makes independently rebuilt policies retain the same
// session-wide execution witness. Snapshots already share their guard.
func (s *Sandbox) ShareExecutionGuard(other *Sandbox) {
	if s == nil || other == nil || s == other {
		return
	}
	other.mu.RLock()
	guard := other.executionGuard
	other.mu.RUnlock()
	if guard == nil {
		guard = &ExecutionGuard{}
	}
	s.mu.Lock()
	s.executionGuard = guard
	s.mu.Unlock()
}

// PolicyTightened reports whether next removes any path, network, or local
// service access granted by current. It is deliberately conservative around
// paths: if root containment cannot be established lexically, it reports a
// tightening and lets the execution guard decide whether the update can be
// confirmed safely.
func PolicyTightened(current, next *Sandbox) bool {
	if current == nil || next == nil {
		return current != next
	}
	old := current.Snapshot()
	newPolicy := next.Snapshot()
	if old.AllowNetwork && !newPolicy.AllowNetwork {
		return true
	}
	oldWrite := append([]string{old.Workspace}, old.AdditionalDirs...)
	newWrite := append([]string{newPolicy.Workspace}, newPolicy.AdditionalDirs...)
	for _, root := range oldWrite {
		if !rootCovered(root, newWrite) {
			return true
		}
	}
	for _, denied := range newPolicy.DisallowedDirs {
		if !rootCovered(denied, old.DisallowedDirs) {
			return true
		}
	}
	for _, protected := range newPolicy.ProtectedDirs {
		if !rootCovered(protected, old.ProtectedDirs) {
			return true
		}
	}
	for _, entry := range newPolicy.ProtectedEntries {
		found := false
		for _, previous := range old.ProtectedEntries {
			if entry == previous {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	for _, readOnly := range newPolicy.ReadOnlyDirs {
		if rootCovered(readOnly, old.ReadOnlyDirs) {
			continue
		}
		for _, writable := range oldWrite {
			if pathWithin(writable, readOnly) || pathWithin(readOnly, writable) {
				return true
			}
		}
	}
	for _, service := range old.LocalServices {
		found := false
		for _, candidate := range newPolicy.LocalServices {
			if service == candidate {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

func rootCovered(root string, allowed []string) bool {
	root = filepath.Clean(root)
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	for _, candidate := range allowed {
		candidate = filepath.Clean(candidate)
		if abs, err := filepath.Abs(candidate); err == nil {
			candidate = abs
		}
		rel, err := filepath.Rel(candidate, root)
		if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// LocalService describes a localhost-only network operation. Seatbelt's host
// token is "localhost" and only accepts a port selector; it cannot enforce a
// literal 127.0.0.1 interface. Direction is "connect" or "listen" and
// Protocol is "tcp" or "udp".
type LocalService struct {
	Direction string
	Protocol  string
	Port      uint16
}

// Limits are per-process resource bounds applied as shell ulimit prefixes.
type Limits struct {
	CPUSeconds int `json:"cpu_seconds"`
	MemoryMB   int `json:"memory_mb"`
	FileSizeMB int `json:"file_size_mb"`
	MaxFiles   int `json:"max_files"`
	MaxCoreKB  int `json:"max_core_kb"`
	StackMB    int `json:"stack_mb"`
	MaxProcs   int `json:"max_procs"`
}

func New(workspace string) *Sandbox {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		abs = workspace
	}
	abs = filepath.Clean(abs)
	abs = canonicalRoot(abs)
	return &Sandbox{Workspace: abs, executionGuard: &ExecutionGuard{}}
}

// Snapshot makes a detached copy of the current policy and its revision.
func (s *Sandbox) Snapshot() *Sandbox {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := &Sandbox{
		Workspace: s.Workspace, Revision: s.Revision,
		executionGuard: s.executionGuard,
		AdditionalDirs: append([]string(nil), s.AdditionalDirs...), ReadOnlyDirs: append([]string(nil), s.ReadOnlyDirs...),
		DisallowedDirs: append([]string(nil), s.DisallowedDirs...), ProtectedDirs: append([]string(nil), s.ProtectedDirs...),
		ProtectedEntries: append([]string(nil), s.ProtectedEntries...),
		ScratchDirs:      append([]string(nil), s.ScratchDirs...), ExecutionReadRoots: append([]string(nil), s.ExecutionReadRoots...),
		ExecutionScratchDirs: append([]string(nil), s.ExecutionScratchDirs...), AllowNetwork: s.AllowNetwork,
		LocalServices: append([]LocalService(nil), s.LocalServices...),
	}
	if s.Limits != nil {
		limits := *s.Limits
		out.Limits = &limits
	}
	return out
}

func (s *Sandbox) SetAllowNetwork(allow bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.AllowNetwork != allow {
		s.AllowNetwork = allow
		s.Revision++
	}
	s.mu.Unlock()
}

func (s *Sandbox) NetworkAllowed() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AllowNetwork
}

func (s *Sandbox) AddDir(dir string)           { s.addRoot(&s.AdditionalDirs, dir) }
func (s *Sandbox) AddReadOnlyDir(dir string)   { s.addRoot(&s.ReadOnlyDirs, dir) }
func (s *Sandbox) AddDisallowedDir(dir string) { s.addRoot(&s.DisallowedDirs, dir) }
func (s *Sandbox) AddScratchDir(dir string)    { s.addRoot(&s.ScratchDirs, dir) }

// AddProtectedDir preserves the caller-visible entry as well as its resolved
// target. A symlink to a control file must not let a tool unlink the symlink
// itself, even though reads and writes resolve to the target inode.
func (s *Sandbox) AddProtectedDir(dir string) {
	if s == nil || dir == "" {
		return
	}
	logical, err := s.Resolve(dir)
	if err != nil {
		return
	}
	canonicalEntry := filepath.Join(canonicalRoot(filepath.Dir(logical)), filepath.Base(logical))
	resolved := canonicalRoot(logical)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, root := range []string{logical, canonicalEntry, resolved} {
		found := false
		for _, existing := range s.ProtectedDirs {
			if existing == root {
				found = true
				break
			}
		}
		if !found {
			s.ProtectedDirs = append(s.ProtectedDirs, root)
			s.Revision++
		}
	}
}

// AddProtectedEntry prevents replacement of a directory entry, such as a
// symlinked .git marker, without denying writes to its target directory.
func (s *Sandbox) AddProtectedEntry(path string) {
	if s == nil || path == "" {
		return
	}
	logical, err := s.Resolve(path)
	if err != nil {
		return
	}
	canonicalEntry := filepath.Join(canonicalRoot(filepath.Dir(logical)), filepath.Base(logical))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range []string{logical, canonicalEntry} {
		found := false
		for _, existing := range s.ProtectedEntries {
			if existing == entry {
				found = true
				break
			}
		}
		if !found {
			s.ProtectedEntries = append(s.ProtectedEntries, entry)
			s.Revision++
		}
	}
}

func (s *Sandbox) AddLocalService(service LocalService) error {
	if s == nil {
		return fmt.Errorf("sandbox: policy is unavailable")
	}
	if service.Direction != "connect" && service.Direction != "listen" {
		return fmt.Errorf("sandbox: local service direction must be connect or listen")
	}
	if service.Protocol != "tcp" && service.Protocol != "udp" {
		return fmt.Errorf("sandbox: local service protocol must be tcp or udp")
	}
	if service.Port == 0 {
		return fmt.Errorf("sandbox: local service requires an explicit port")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.LocalServices {
		if existing == service {
			return nil
		}
	}
	s.LocalServices = append(s.LocalServices, service)
	s.Revision++
	return nil
}

// AddExecutionReadRoot adds a per-invocation runtime root on a detached policy
// snapshot. It deliberately does not change the durable policy revision.
func (s *Sandbox) AddExecutionReadRoot(dir string) {
	if s == nil {
		return
	}
	abs := s.absDir(dir)
	if abs == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.ExecutionReadRoots {
		if existing == abs {
			return
		}
	}
	s.ExecutionReadRoots = append(s.ExecutionReadRoots, abs)
}

// AddExecutionScratchDir grants a dedicated scratch directory to one
// detached invocation without changing the durable policy revision.
func (s *Sandbox) AddExecutionScratchDir(dir string) {
	if s == nil {
		return
	}
	abs := s.absDir(dir)
	if abs == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.ExecutionScratchDirs {
		if existing == abs {
			return
		}
	}
	s.ExecutionScratchDirs = append(s.ExecutionScratchDirs, abs)
}

func (s *Sandbox) addRoot(target *[]string, dir string) {
	if s == nil {
		return
	}
	abs := s.absDir(dir)
	if abs == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range *target {
		if existing == abs {
			return
		}
	}
	*target = append(*target, abs)
	s.Revision++
}

func (s *Sandbox) SetScratchDir(dir string) {
	if s == nil {
		return
	}
	abs := s.absDir(dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	if abs == "" {
		s.ScratchDirs = nil
	} else {
		s.ScratchDirs = []string{abs}
	}
	s.Revision++
}

func (s *Sandbox) absDir(dir string) string {
	if dir == "" {
		return ""
	}
	abs := dir
	if !filepath.IsAbs(abs) {
		base := s.Workspace
		if base == "" {
			base, _ = os.Getwd()
		}
		abs = filepath.Join(base, abs)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return ""
	}
	abs = filepath.Clean(abs)
	return canonicalRoot(abs)
}

// Resolve returns a cleaned absolute path without deciding whether it is
// authorized. Call ResolveRead or ResolveWrite before file access.
func (s *Sandbox) Resolve(path string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("sandbox: policy is unavailable")
	}
	if path == "" {
		return "", fmt.Errorf("sandbox: empty path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.Workspace, path)
	}
	return filepath.Clean(path), nil
}

func (s *Sandbox) ResolveRead(path string) (string, error) {
	abs, err := s.Resolve(path)
	if err != nil {
		return "", err
	}
	if !s.InReadScope(abs) {
		return "", fmt.Errorf("sandbox: read of %s is denied by policy", path)
	}
	return abs, nil
}

func (s *Sandbox) ResolveWrite(path string) (string, error) {
	abs, err := s.Resolve(path)
	if err != nil {
		return "", err
	}
	if s.isProtected(abs) {
		return "", fmt.Errorf("sandbox: write to protected runtime location %s", path)
	}
	if s.isReadOnly(abs) {
		return "", fmt.Errorf("sandbox: write to read-only location %s", path)
	}
	if !s.InWorkspace(abs) {
		return "", fmt.Errorf("sandbox: write to %s is outside the workspace or an authorized writable directory", path)
	}
	return abs, nil
}

func (s *Sandbox) InWorkspace(path string) bool {
	if s == nil || s.blockedByDisallowed(path) {
		return false
	}
	resolved, err := resolveExisting(path)
	if err != nil {
		resolved = path
	}
	if s.blockedByDisallowed(resolved) {
		return false
	}
	s.mu.RLock()
	roots := append([]string{s.Workspace}, s.AdditionalDirs...)
	roots = append(roots, s.ScratchDirs...)
	s.mu.RUnlock()
	for _, root := range roots {
		if pathWithin(root, resolved) {
			return true
		}
	}
	return false
}

func (s *Sandbox) InReadScope(path string) bool {
	if s == nil || s.blockedByDisallowed(path) {
		return false
	}
	resolved, err := resolveExisting(path)
	if err != nil {
		resolved = path
	}
	// A hard link outside a disallowed tree can name the same inode as a file
	// inside it. The application file tools cannot reverse-map the inode
	// safely, so reject multiply-linked regular files when sensitive roots are
	// present rather than treating the alias pathname as sufficient authority.
	s.mu.RLock()
	disallowed := append([]string(nil), s.DisallowedDirs...)
	s.mu.RUnlock()
	if (isMultiplyLinkedRegularFile(path) || isMultiplyLinkedRegularFile(resolved)) && hasExistingProtectedRoot(disallowed) {
		return false
	}
	return !s.blockedByDisallowed(resolved)
}

func (s *Sandbox) isProtected(path string) bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	roots := append([]string(nil), s.ProtectedDirs...)
	s.mu.RUnlock()
	resolved, err := resolveExisting(path)
	if err != nil {
		resolved = path
	}
	for _, root := range roots {
		if pathWithin(root, path) || pathWithin(root, resolved) {
			return true
		}
	}
	return isMultiplyLinkedRegularFile(path) && hasExistingProtectedRoot(roots)
}

func (s *Sandbox) isReadOnly(path string) bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	roots := append([]string(nil), s.ReadOnlyDirs...)
	s.mu.RUnlock()
	resolved, err := resolveExisting(path)
	if err != nil {
		resolved = path
	}
	for _, root := range roots {
		if pathWithin(root, path) || pathWithin(root, resolved) {
			return true
		}
	}
	return isMultiplyLinkedRegularFile(path) && hasExistingProtectedRoot(roots)
}

func hasExistingProtectedRoot(roots []string) bool {
	for _, root := range roots {
		if _, err := os.Lstat(root); err == nil {
			return true
		}
	}
	return false
}

func isMultiplyLinkedRegularFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink > 1
}

func (s *Sandbox) blockedByDisallowed(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, root := range s.DisallowedDirs {
		if pathWithin(root, path) {
			return true
		}
	}
	return false
}

func pathWithin(root, child string) bool {
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func resolveExisting(path string) (string, error) {
	existing, suffix := path, ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing ancestor to resolve")
		}
		suffix = filepath.Join(filepath.Base(existing), suffix)
		existing = parent
	}
}

func (s *Sandbox) Prefix() string {
	if s == nil || s.Limits == nil {
		return ""
	}
	l := s.Limits
	var parts []string
	if l.CPUSeconds > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -t %d", l.CPUSeconds))
	}
	if l.MemoryMB > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -v %d", l.MemoryMB*1024))
	}
	if l.FileSizeMB > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -f %d", l.FileSizeMB*1024))
	}
	if l.MaxFiles > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -n %d", l.MaxFiles))
	}
	if l.MaxCoreKB > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -c %d", l.MaxCoreKB))
	}
	if l.StackMB > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -s %d", l.StackMB*1024))
	}
	if l.MaxProcs > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -u %d", l.MaxProcs))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " && ") + " &&"
}

func VerifyDir(workspace string) error {
	info, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %s is not a directory", workspace)
	}
	return nil
}

func (s *Sandbox) BackendError() error {
	if s == nil {
		return fmt.Errorf("sandbox: policy is unavailable")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("sandbox: local execution requires the macOS Seatbelt backend (running on %s)", runtime.GOOS)
	}
	info, err := os.Stat(BackendPath)
	if err != nil || info.Mode()&0111 == 0 {
		if err == nil {
			err = fmt.Errorf("not executable")
		}
		return fmt.Errorf("sandbox: required backend %s is unavailable: %w", BackendPath, err)
	}
	return nil
}

func (s *Sandbox) Profile() (string, error) {
	if err := s.BackendError(); err != nil {
		return "", err
	}
	s.mu.RLock()
	workspace := s.Workspace
	additional := append([]string(nil), s.AdditionalDirs...)
	readOnly := append([]string(nil), s.ReadOnlyDirs...)
	disallowed := append([]string(nil), s.DisallowedDirs...)
	protected := append([]string(nil), s.ProtectedDirs...)
	protectedEntries := append([]string(nil), s.ProtectedEntries...)
	scratch := append([]string(nil), s.ScratchDirs...)
	executionScratch := append([]string(nil), s.ExecutionScratchDirs...)
	executionReadRoots := append([]string(nil), s.ExecutionReadRoots...)
	allowNetwork := s.AllowNetwork
	localServices := append([]LocalService(nil), s.LocalServices...)
	s.mu.RUnlock()
	if workspace == "" {
		return "", fmt.Errorf("sandbox: profile has no workspace root")
	}
	workspace = canonicalRoot(workspace)
	additional = canonicalRoots(additional)
	readOnly = canonicalRoots(readOnly)
	disallowed = canonicalRoots(disallowed)
	protected = cleanRoots(protected)
	protectedEntries = cleanRoots(protectedEntries)
	scratch = canonicalRoots(scratch)
	executionScratch = canonicalRoots(executionScratch)
	executionReadRoots = canonicalRoots(executionReadRoots)

	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n(import \"system.sb\")\n")
	// Like Codex workspace-write, commands may inspect host files unless an
	// explicit deny applies. Write access remains limited to authorized roots.
	b.WriteString("(allow file-read*)\n")
	for _, root := range disallowed {
		fmt.Fprintf(&b, "(deny file-read* file-write* (literal %s))\n", sbplQuote(root))
		fmt.Fprintf(&b, "(deny file-read* file-write* (subpath %s))\n", sbplQuote(root))
	}
	for _, root := range protected {
		fmt.Fprintf(&b, "(deny file-write* (literal %s))\n", sbplQuote(root))
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", sbplQuote(root))
		fmt.Fprintf(&b, "(deny file-write-unlink (regex #\"^%s$\"))\n", sbplRegexEscape(root))
	}
	for _, entry := range protectedEntries {
		fmt.Fprintf(&b, "(deny file-write-unlink (regex #\"^%s$\"))\n", sbplRegexEscape(entry))
	}
	for _, root := range readOnly {
		fmt.Fprintf(&b, "(deny file-write* (literal %s))\n", sbplQuote(root))
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", sbplQuote(root))
	}
	writeDenied := append(append(append([]string(nil), disallowed...), protected...), readOnly...)
	writableRoots := append(append(append([]string{workspace}, additional...), scratch...), executionScratch...)
	for _, ancestor := range protectedWritableAncestors(writableRoots, writeDenied) {
		fmt.Fprintf(&b, "(deny file-write-unlink (require-all (literal %s) (vnode-type DIRECTORY)))\n", sbplQuote(ancestor))
	}
	fmt.Fprintf(&b, "(allow file-read* (require-all (literal %s)%s))\n", sbplQuote("/private/var/select/sh"), sbplDenyPredicates(disallowed))

	for _, root := range append(toolchainRoots(), executionReadRoots...) {
		fmt.Fprintf(&b, "(allow file-read* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(disallowed))
	}
	for _, root := range []string{"/bin", "/usr/bin", "/sbin", "/usr/sbin"} {
		fmt.Fprintf(&b, "(allow file-read-metadata file-test-existence (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(disallowed))
	}
	// Keep exact-ancestor metadata checks for paths used by the execution
	// adapter. Broad reads already permit directory inspection; these rules
	// make the intended traversal permissions explicit.
	metadataRoots := append([]string{workspace}, additional...)
	metadataRoots = append(metadataRoots, readOnly...)
	metadataRoots = append(metadataRoots, scratch...)
	metadataRoots = append(metadataRoots, executionScratch...)
	metadataRoots = append(metadataRoots, executionReadRoots...)
	for _, parent := range exactAncestors(metadataRoots) {
		fmt.Fprintf(&b, "(allow file-read-metadata file-test-existence (require-all (literal %s)%s))\n", sbplQuote(parent), sbplDenyPredicates(disallowed))
	}
	for _, root := range append([]string{workspace}, additional...) {
		fmt.Fprintf(&b, "(allow file-read* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(disallowed))
		fmt.Fprintf(&b, "(allow file-write* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(writeDenied))
	}
	for _, root := range readOnly {
		fmt.Fprintf(&b, "(allow file-read* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(disallowed))
	}
	for _, root := range append(scratch, executionScratch...) {
		fmt.Fprintf(&b, "(allow file-read* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(disallowed))
		fmt.Fprintf(&b, "(allow file-write* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(writeDenied))
	}
	b.WriteString("(allow process-fork)\n(allow process-exec)\n")
	if allowNetwork {
		// External networking excludes host-local services. Unix-domain sockets
		// and named endpoints remain denied because these rules match IP-based
		// TCP and UDP only. A local DNS stub, if needed, must be authorized
		// separately as a local service; general egress cannot imply it.
		b.WriteString("(allow network-outbound (require-all (remote tcp) (require-not (remote ip \"localhost:*\"))))\n")
		b.WriteString("(allow network-outbound (require-all (remote udp) (require-not (remote ip \"localhost:*\"))))\n")
	}
	for _, service := range localServices {
		selector := fmt.Sprintf("localhost:%d", service.Port)
		if service.Direction == "connect" {
			// Seatbelt's protocol+host selector is a single conjunctive token.
			// Combining separate (remote tcp) and (remote ip ...) predicates
			// with require-all looks precise but the kernel rejects the rule for
			// real local connections.
			fmt.Fprintf(&b, "(allow network-outbound (remote %s %s))\n", service.Protocol, sbplQuote(selector))
			continue
		}
		fmt.Fprintf(&b, "(allow network-bind (require-all (local %s) (local ip %s)))\n", service.Protocol, sbplQuote(selector))
		fmt.Fprintf(&b, "(allow network-inbound (local %s %s))\n", service.Protocol, sbplQuote(selector))
	}
	// A writable root is an authority boundary for this invocation. Its
	// contents remain writable, but the directory entry itself must not be
	// renamed or removed, even when its parent is another writable root.
	for _, root := range canonicalRoots(writableRoots) {
		fmt.Fprintf(&b, "(deny file-write-unlink (require-all (literal %s) (vnode-type DIRECTORY)))\n", sbplQuote(root))
	}
	return b.String(), nil
}

func exactAncestors(roots []string) []string {
	seen := map[string]bool{}
	var parents []string
	for _, root := range roots {
		for parent := filepath.Dir(filepath.Clean(root)); parent != "."; parent = filepath.Dir(parent) {
			if !seen[parent] {
				seen[parent] = true
				parents = append(parents, parent)
			}
			if parent == string(filepath.Separator) {
				break
			}
		}
	}
	return parents
}

// An excluded path can be moved out of its exclusion by renaming any writable
// ancestor. Deny unlinking those directory entries while leaving writes inside
// the directories available.
func protectedWritableAncestors(writableRoots, excludedRoots []string) []string {
	seen := map[string]bool{}
	var ancestors []string
	for _, writable := range writableRoots {
		for _, excluded := range excludedRoots {
			if !pathWithin(writable, excluded) || writable == excluded {
				continue
			}
			for parent := filepath.Dir(excluded); pathWithin(writable, parent); parent = filepath.Dir(parent) {
				if !seen[parent] {
					seen[parent] = true
					ancestors = append(ancestors, parent)
				}
				if parent == writable {
					break
				}
			}
		}
	}
	sort.Strings(ancestors)
	return ancestors
}

func toolchainRoots() []string {
	var roots []string
	for _, root := range []string{
		"/usr/local/go",
		"/Applications/Xcode.app/Contents/Developer/Toolchains/XcodeDefault.xctoolchain",
		"/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs",
		"/Library/Developer/CommandLineTools",
		"/Library/Developer/CommandLineTools/usr",
		"/Library/Developer/CommandLineTools/SDKs",
	} {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			roots = append(roots, canonicalRoot(root))
		}
	}
	return roots
}

func canonicalRoot(path string) string {
	path = filepath.Clean(path)
	// A protected path may not exist yet. Resolve its nearest existing parent
	// too, so aliases such as /tmp -> /private/tmp cannot bypass Seatbelt.
	if resolved, err := resolveExisting(path); err == nil {
		return resolved
	}
	return path
}

func canonicalRoots(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		path = canonicalRoot(path)
		if path != "" && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

func cleanRoots(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		path = filepath.Clean(path)
		if path != "." && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

func sbplQuote(path string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(path, `\`, `\\`), `"`, `\"`) + `"`
}

func sbplRegexEscape(path string) string {
	return strings.ReplaceAll(regexp.QuoteMeta(path), `"`, `\"`)
}

func sbplDenyPredicates(roots []string) string {
	var b strings.Builder
	for _, root := range roots {
		fmt.Fprintf(&b, " (require-not (literal %s))", sbplQuote(root))
		fmt.Fprintf(&b, " (require-not (subpath %s))", sbplQuote(root))
	}
	return b.String()
}

func PrepareCommand(s *Sandbox, command string) (string, error) {
	return PrepareCommandContext(context.Background(), s, command)
}

func PrepareCommandContext(ctx context.Context, s *Sandbox, command string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("sandbox: policy is unavailable")
	}
	if err := CheckInteractive(command); err != nil {
		return "", err
	}
	if prefix := s.Prefix(); prefix != "" {
		command = prefix + " " + command
	}
	profile, err := s.Profile()
	if err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, BackendPath, "-p", profile, "--", "/bin/sh", "-c", "true")
	if output, err := probe.CombinedOutput(); err != nil {
		return "", fmt.Errorf("sandbox: Seatbelt rejected the profile: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return command, nil
}

var interactivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(vim|vimdiff|nvim|vi|nano|emacs|pico|less)\b`),
	regexp.MustCompile(`(?i)(^|[\s;&|])read(?:[\s;&|]|$)`),
	regexp.MustCompile(`(?i)\b(top|htop|btop)\b`),
	regexp.MustCompile(`git\s+(rebase\s+-i|add\s+-p|commit\s+--amend\s+-i|clean\s+-i)`),
	regexp.MustCompile(`(?i)\b(ssh)\b\s.*-t\b`),
}

func CheckInteractive(command string) error {
	command = strings.TrimSpace(command)
	for _, pattern := range interactivePatterns {
		if pattern.MatchString(command) {
			return fmt.Errorf("sandbox: command %q needs an interactive terminal; run it yourself instead", command)
		}
	}
	return nil
}
