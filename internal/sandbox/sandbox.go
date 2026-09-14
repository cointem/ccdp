// Package sandbox confines tool file access to the workspace. It implements a
// pragmatic, cross-platform sandbox (Codex uses kernel-level isolation; on a
// plain Go TUI we enforce containment in userspace):
//
//   - All file tools resolve paths through Sandbox.Resolve, which rejects
//     escapes outside the workspace root.
//   - Writes outside the workspace are always denied in "confine" mode and
//     reads too in "strict" mode.
//   - Bash commands are confined to the workspace working directory; in strict
//     mode a command policy blocks obvious escapes.
//
// The sandbox never replaces the permissions gate: it is a hard containment
// layer underneath the approval system.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Mode selects how aggressively the sandbox confines tool access.
type Mode string

const (
	ModeConfine Mode = "confine" // file writes confined to workspace; reads allowed outside
	ModeStrict  Mode = "strict"  // every file access confined to workspace
	ModeNone    Mode = "none"    // no containment (Bash-only workflows)
)

// darwinShellSelectorPath is the macOS path consulted by /bin/sh when the
// system's selectable shell link is resolved. It is intentionally an exact
// read exception, not a /private/var subtree grant: strict commands otherwise
// fail before their configured workspace policy can even be applied.
const darwinShellSelectorPath = "/private/var/select/sh"

// ValidModes lists selectable sandbox modes.
var ValidModes = []Mode{ModeConfine, ModeStrict, ModeNone}

// ParseMode converts a string to a Mode.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeConfine, ModeStrict, ModeNone:
		return Mode(s), nil
	}
	return "", fmt.Errorf("unknown sandbox mode %q (want confine|strict|none)", s)
}

// Sandbox is a workspace-bound path resolver and process guard. It is safe for
// concurrent use: tool goroutines resolve paths while the TUI/agent loop adds
// directories or switches the mode.
type Sandbox struct {
	Workspace string // absolute workspace root
	Mode      Mode

	// AdditionalDirs are extra absolute directories treated like the workspace:
	// writable in every mode, readable in strict mode. (/add-dir, Claude Code's
	// additionalDirectories.)
	AdditionalDirs []string

	// DisallowedDirs are absolute directories the sandbox always blocks, even
	// when they fall inside the workspace or an additional dir. (/disallowed-dir)
	DisallowedDirs []string

	// ProtectedDirs are runtime-owned control/data roots. Ordinary file tools
	// cannot write them, even in ModeNone or when an additional directory also
	// grants the same path. The session owner may still use its own adapters.
	ProtectedDirs []string

	// ScratchDirs are explicit temporary roots granted to strict subprocesses.
	// They default to none: granting all of /tmp would let one session read or
	// overwrite another session's temporary data. Runtime should set an
	// owner-specific scratch directory when a tool needs one.
	ScratchDirs []string

	// Limits, when non-zero, are applied to every Bash tool invocation as
	// shell ulimit prefixes (Codex's rlimit enforcement, userspace flavor).
	Limits *Limits

	// AllowNetwork permits outbound network commands in strict mode. When
	// false, strict mode blocks obvious network clients.
	AllowNetwork bool

	mu sync.RWMutex // guards mutable policy fields
}

// SetMode switches the sandbox mode at runtime (safe for concurrent use).
func (s *Sandbox) SetMode(m Mode) {
	s.mu.Lock()
	s.Mode = m
	s.mu.Unlock()
}

// CurrentMode returns the sandbox mode (safe for concurrent use).
func (s *Sandbox) CurrentMode() Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Mode
}

// SetAllowNetwork changes the network capability used by both userspace
// checks and the Darwin profile. Keep this behind the sandbox mutex so a
// policy reload cannot race profile generation.
func (s *Sandbox) SetAllowNetwork(allow bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.AllowNetwork = allow
	s.mu.Unlock()
}

// NetworkAllowed returns the current network capability.
func (s *Sandbox) NetworkAllowed() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AllowNetwork
}

// AddDir grants access to an extra directory at runtime. Relative paths are
// resolved against the workspace.
func (s *Sandbox) AddDir(dir string) {
	abs := s.absDir(dir)
	if abs == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.AdditionalDirs {
		if d == abs {
			return
		}
	}
	s.AdditionalDirs = append(s.AdditionalDirs, abs)
}

// AddDisallowedDir blocks a directory at runtime (absolute resolution like
// AddDir). A disallowed dir overrides the workspace and additional dirs.
func (s *Sandbox) AddDisallowedDir(dir string) {
	abs := s.absDir(dir)
	if abs == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.DisallowedDirs {
		if d == abs {
			return
		}
	}
	s.DisallowedDirs = append(s.DisallowedDirs, abs)
}

// AddProtectedDir marks an owner-controlled directory that ordinary tools may
// not write. Unlike a disallowed directory, it remains readable in confine
// mode so diagnostics do not need a second filesystem policy.
func (s *Sandbox) AddProtectedDir(dir string) {
	if s == nil {
		return
	}
	abs := s.absDir(dir)
	if abs == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.ProtectedDirs {
		if d == abs {
			return
		}
	}
	s.ProtectedDirs = append(s.ProtectedDirs, abs)
}

// SetScratchDir replaces the explicit temporary roots used by strict process
// profiles. Relative paths are resolved against the workspace.
func (s *Sandbox) SetScratchDir(dir string) {
	if s == nil {
		return
	}
	abs := s.absDir(dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	if abs == "" {
		s.ScratchDirs = nil
		return
	}
	s.ScratchDirs = []string{abs}
}

// AddScratchDir appends an explicit temporary root to strict profiles.
func (s *Sandbox) AddScratchDir(dir string) {
	if s == nil {
		return
	}
	abs := s.absDir(dir)
	if abs == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.ScratchDirs {
		if d == abs {
			return
		}
	}
	s.ScratchDirs = append(s.ScratchDirs, abs)
}

func (s *Sandbox) absDir(dir string) string {
	if dir == "" {
		return ""
	}
	abs := dir
	if !filepath.IsAbs(abs) {
		// /add-dir is workspace-relative, not process-cwd-relative. This is
		// important when the runtime changes workspace or starts from a GUI
		// process whose cwd is unrelated to the session.
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
	// Symlink-resolve so comparisons are stable (e.g. /var → /private/var on
	// macOS), matching how New resolves the workspace root.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs
}

// blockedByDisallowed reports whether abs falls inside any disallowed dir.
func (s *Sandbox) blockedByDisallowed(abs string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.DisallowedDirs {
		if pathWithin(d, abs) {
			return true
		}
	}
	return false
}

// pathWithin reports whether child is inside (or equal to) root, using a
// stable symlink-resolved comparison.
func pathWithin(root, child string) bool {
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		resolved, rerr := resolveExisting(child)
		if rerr != nil {
			return false
		}
		rel2, err2 := filepath.Rel(root, resolved)
		return err2 == nil && rel2 != ".." && !strings.HasPrefix(rel2, ".."+string(filepath.Separator))
	}
	return true
}

// Limits are per-command resource bounds applied as ulimit settings.
// Zero values mean "unbounded".
type Limits struct {
	CPUSeconds int `json:"cpu_seconds"`  // -t: max CPU seconds
	MemoryMB   int `json:"memory_mb"`    // -v: address space in MB
	FileSizeMB int `json:"file_size_mb"` // -f: max file size in MB
	MaxFiles   int `json:"max_files"`    // -n: max open file descriptors
	MaxCoreKB  int `json:"max_core_kb"`  // -c: core dump size in KB
	StackMB    int `json:"stack_mb"`     // -s: stack size in MB
	MaxProcs   int `json:"max_procs"`    // -u: max user processes
}

// Prefix renders the shell ulimit prefix enforcing the configured limits, or ""
// when no limits are set. The command runs inside the same shell so ulimit
// applies to it and every child.
func (s *Sandbox) Prefix() string {
	if s == nil || s.Limits == nil {
		return ""
	}
	l := s.Limits
	var parts []string
	if l.CPUSeconds > 0 {
		parts = append(parts, fmt.Sprintf("-t %d", l.CPUSeconds))
	}
	if l.MemoryMB > 0 {
		parts = append(parts, fmt.Sprintf("-v %d", l.MemoryMB*1024))
	}
	if l.FileSizeMB > 0 {
		parts = append(parts, fmt.Sprintf("-f %d", l.FileSizeMB*1024))
	}
	if l.MaxFiles > 0 {
		parts = append(parts, fmt.Sprintf("-n %d", l.MaxFiles))
	}
	if l.MaxCoreKB > 0 {
		parts = append(parts, fmt.Sprintf("-c %d", l.MaxCoreKB))
	}
	if l.StackMB > 0 {
		parts = append(parts, fmt.Sprintf("-s %d", l.StackMB*1024))
	}
	if l.MaxProcs > 0 {
		parts = append(parts, fmt.Sprintf("-u %d", l.MaxProcs))
	}
	if len(parts) == 0 {
		return ""
	}
	return "ulimit " + strings.Join(parts, " ") + " &&"
}

// New builds a sandbox for the given workspace and mode. The workspace root is
// symlink-resolved once so containment comparisons are stable (e.g. /var →
// /private/var on macOS).
func New(workspace string, mode Mode) *Sandbox {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		abs = workspace
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &Sandbox{Workspace: abs, Mode: mode}
}

// Resolve cleans p against the workspace and returns an absolute path. For a
// relative path, p is joined to the workspace. It performs no containment
// enforcement by itself — call ResolveRead or ResolveWrite for that. The
// returned path is the caller-visible cleaned absolute path; containment is
// checked against the symlink-resolved form so symlinks can't smuggle access
// outside the workspace.
func (s *Sandbox) Resolve(p string) (string, error) {
	if s == nil {
		if filepath.IsAbs(p) {
			return filepath.Clean(p), nil
		}
		return filepath.Join(".", p), nil
	}
	if s.CurrentMode() == ModeNone {
		if filepath.IsAbs(p) {
			return filepath.Clean(p), nil
		}
		return filepath.Join(s.Workspace, p), nil
	}
	if p == "" {
		return "", fmt.Errorf("sandbox: empty path")
	}

	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.Workspace, abs)
	}
	return filepath.Clean(abs), nil
}

// ResolveWrite is like Resolve but additionally enforces write confinement:
// writes are confined to the workspace in every mode except none.
func (s *Sandbox) ResolveWrite(p string) (string, error) {
	if s == nil {
		return s.Resolve(p)
	}
	abs, err := s.Resolve(p)
	if err != nil {
		return "", err
	}
	if s.isProtected(abs) {
		return "", fmt.Errorf("sandbox: write to protected runtime location %s", p)
	}
	if s.CurrentMode() == ModeNone {
		return abs, nil
	}
	if !s.InWorkspace(abs) {
		return "", fmt.Errorf("sandbox: write to %s is outside workspace %s", p, s.Workspace)
	}
	return abs, nil
}

func (s *Sandbox) isProtected(abs string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	protected := append([]string(nil), s.ProtectedDirs...)
	s.mu.RUnlock()
	for _, root := range protected {
		if pathWithin(root, abs) {
			return true
		}
	}
	// A hard link has no path component for Resolve/InWorkspace to resolve. If
	// runtime control roots exist, do not permit an existing multiply-linked
	// regular file to be overwritten through a workspace alias. This is a
	// conservative write rule: ordinary model files can still be read, while a
	// control file cannot be modified by first linking it into the workspace.
	if isMultiplyLinkedRegularFile(abs) && hasExistingProtectedRoot(protected) {
		return true
	}
	return false
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

// ResolveRead enforces strict-mode read confinement.
func (s *Sandbox) ResolveRead(p string) (string, error) {
	if s == nil {
		return s.Resolve(p)
	}
	mode := s.CurrentMode()
	if mode == ModeNone {
		return s.Resolve(p)
	}
	abs, err := s.Resolve(p)
	if err != nil {
		return "", err
	}
	if mode == ModeStrict && !s.InWorkspace(abs) {
		return "", fmt.Errorf("sandbox: read of %s is outside workspace %s (strict mode)", p, s.Workspace)
	}
	return abs, nil
}

// StrictBackendError reports why a strict process backend cannot be used.
// Strict is intentionally Darwin-only; userspace command regexes are not
// presented as an equivalent security boundary on other platforms.
func (s *Sandbox) StrictBackendError() error {
	if s == nil || s.CurrentMode() != ModeStrict {
		return nil
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("sandbox: strict execution requires the macOS sandbox-exec backend (running on %s)", runtime.GOOS)
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		return fmt.Errorf("sandbox: strict execution backend /usr/bin/sandbox-exec is unavailable: %w", err)
	}
	return nil
}

// InWorkspace reports whether abs refers to a location inside the workspace or
// an additional dir, resolving symlinks in the deepest existing ancestor to
// defeat escape attempts through links (e.g. `ln -s /etc ./evil` followed by a
// write to evil/passwd). Disallowed dirs always win.
func (s *Sandbox) InWorkspace(abs string) bool {
	root := filepath.Clean(s.Workspace)
	withinRoot := func(p string) bool {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return false
		}
		if rel == "." {
			return true
		}
		return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	// Resolve symlinks (longest existing ancestor) so a link can't smuggle a
	// lexically-inside path outside the workspace — or into a disallowed dir.
	// Not-yet-created files resolve through their existing parents.
	resolved, rerr := resolveExisting(abs)
	if rerr != nil {
		resolved = abs // nothing resolvable; judge the lexical path
	}
	if s.blockedByDisallowed(abs) || s.blockedByDisallowed(resolved) {
		return false
	}
	// Additional dirs are treated exactly like the workspace; both the caller's
	// path and its resolved form must land inside the same additional dir.
	s.mu.RLock()
	additional := append([]string(nil), s.AdditionalDirs...)
	additional = append(additional, s.ScratchDirs...)
	s.mu.RUnlock()
	for _, d := range additional {
		if pathWithin(d, abs) && pathWithin(d, resolved) {
			return true
		}
	}
	// The symlink-resolved location decides: a lexical hit that turns out to
	// pass through a link leaving the workspace is not inside.
	return withinRoot(resolved)
}

// resolveExisting resolves symlinks for the longest existing prefix of p so
// checks work for not-yet-created files too.
func resolveExisting(p string) (string, error) {
	existing := p
	suffix := ""
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

// CommandPolicy inspects a shell command for workspace escapes when strict
// sandboxing is active. Returns an error for commands that would operate
// outside the workspace's control.
func (s *Sandbox) CommandPolicy(command string) error {
	if s == nil || s.CurrentMode() != ModeStrict {
		return nil
	}
	c := strings.TrimSpace(command)
	if c == "" {
		return nil
	}

	// Interactive commands cannot be driven non-interactively; refuse them in
	// every sandbox mode via the caller (Claude Code refuses these too).
	if err := CheckInteractive(c); err != nil {
		return err
	}

	// Network clients are blocked in strict mode unless explicitly allowed
	// (set sandbox_allow_network: true in config).
	if !s.NetworkAllowed() {
		for _, pat := range networkPatterns {
			if pat.re.MatchString(c) {
				return fmt.Errorf("sandbox: command %q blocked in strict mode (%s); enable sandbox_allow_network to permit", c, pat.desc)
			}
		}
	}

	// Absolute-path destructive writes and package-manager installs are
	// blocked in strict mode (the user can still approve via /mode).
	for _, pat := range []string{"rm -rf /", "rm -rf ~", "sudo ", "mount ", "umount "} {
		if strings.HasPrefix(c, pat) {
			return fmt.Errorf("sandbox: command %q is blocked in strict mode", c)
		}
	}

	// Changing to the filesystem root then acting is an escape vector.
	for _, seg := range []string{"; cd /", "&& cd /", "cd ~ &&", "cd $HOME"} {
		if strings.Contains(c, seg) {
			return fmt.Errorf("sandbox: command %q escapes the workspace in strict mode", c)
		}
	}
	return nil
}

// Profile returns the Darwin SBPL profile for the current strict policy.
// Explicitly disallowed directories are emitted before any allow roots and
// are also repeated after them; this keeps the deny precedence obvious to
// readers and robust across seatbelt rule-order differences.
func (s *Sandbox) Profile() (string, error) {
	if s == nil || s.CurrentMode() != ModeStrict {
		return "", nil
	}
	if err := s.StrictBackendError(); err != nil {
		return "", err
	}
	s.mu.RLock()
	workspace := s.Workspace
	additional := append([]string(nil), s.AdditionalDirs...)
	disallowed := append([]string(nil), s.DisallowedDirs...)
	protected := append([]string(nil), s.ProtectedDirs...)
	scratch := append([]string(nil), s.ScratchDirs...)
	allowNetwork := s.AllowNetwork
	s.mu.RUnlock()
	if workspace == "" {
		return "", fmt.Errorf("sandbox: strict profile has no workspace root")
	}
	workspace = canonicalRoot(workspace)
	additional = canonicalRoots(additional)
	disallowed = canonicalRoots(disallowed)
	protected = canonicalRoots(protected)
	scratch = canonicalRoots(scratch)

	var b strings.Builder
	b.WriteString("(version 1)\n")
	// Start from the system runtime profile rather than `allow default`: this
	// gives shell/dyld the narrow system read rules it needs without granting
	// arbitrary user-data access.
	b.WriteString("(deny default)\n(import \"system.sb\")\n")
	// Keep an explicit deny for every protected root. The allow rules below also
	// carry require-not predicates, so a root allow can never accidentally
	// re-admit a disallowed subtree even if a future seatbelt rule is reordered.
	for _, d := range disallowed {
		fmt.Fprintf(&b, "(deny file-read* file-write* (subpath %s))\n", sbplQuote(d))
	}
	for _, d := range protected {
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", sbplQuote(d))
	}
	// /bin/sh resolves through this one system selector on macOS. Keep the
	// exception read-only, exact, and subject to user disallowed roots so a
	// caller cannot turn a control-path deny into a broad /private/var allow.
	fmt.Fprintf(&b, "(allow file-read* (require-all (literal %s)%s))\n", sbplQuote(darwinShellSelectorPath), sbplDenyPredicates(disallowed))
	// PATH lookup for an executable such as `sh` needs only directory metadata
	// and existence checks. Permit those checks in the fixed system executable
	// roots, while deliberately withholding file-read-data and subjecting every
	// root to the configured disallow list. This keeps bare command lookup
	// usable under strict mode without turning the system roots into data-read
	// or user-data capabilities.
	for _, root := range [...]string{"/bin", "/usr/bin", "/sbin", "/usr/sbin"} {
		fmt.Fprintf(&b, "(allow file-read-metadata file-test-existence (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(disallowed))
	}
	writeDenied := append(append([]string(nil), disallowed...), protected...)
	// Workspace and explicitly added roots are the only user-data roots. A
	// separate temporary root is available for shell scratch files.
	for _, root := range append([]string{workspace}, additional...) {
		fmt.Fprintf(&b, "(allow file-read* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(disallowed))
		fmt.Fprintf(&b, "(allow file-write* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(writeDenied))
	}
	for _, root := range scratch {
		fmt.Fprintf(&b, "(allow file-read* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(disallowed))
		fmt.Fprintf(&b, "(allow file-write* (require-all (subpath %s)%s))\n", sbplQuote(root), sbplDenyPredicates(writeDenied))
	}
	// Keep process execution available to normal commands while the file and
	// network capabilities remain governed by this profile.
	b.WriteString("(allow process-fork)\n(allow process-exec)\n")
	if allowNetwork {
		// The network capability is intentionally explicit. `network*` is a
		// broad shorthand whose condition semantics vary across macOS releases;
		// exact inbound/outbound actions make the true path auditable and match
		// the platform seatbelt policy.
		b.WriteString("(allow network-outbound)\n(allow network-inbound)\n")
	} else {
		// `(deny default)` already denies network actions. Do not add a broad
		// allow followed by a deny: seatbelt rule ordering and the meaning of
		// the `network*` shorthand vary across macOS releases. The only network
		// exception inherited here is the system.sb syslog socket, which is not
		// an outbound Internet capability.
	}
	return b.String(), nil
}

func canonicalRoot(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func canonicalRoots(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		p = canonicalRoot(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func sbplQuote(path string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(path, `\`, `\\`), `"`, `\"`) + `"`
}

func sbplDenyPredicates(disallowed []string) string {
	var b strings.Builder
	for _, d := range disallowed {
		fmt.Fprintf(&b, " (require-not (subpath %s))", sbplQuote(d))
	}
	return b.String()
}

// PrepareCommand performs policy checks and returns a command ready for
// execution. Strict mode always requires a working Darwin backend; callers
// must not fall back to userspace regex checks after this function errors.
func PrepareCommand(s *Sandbox, command string) (string, error) {
	return PrepareCommandContext(context.Background(), s, command)
}

// PrepareCommandContext is PrepareCommand with a caller-owned cancellation
// boundary for the profile compile probe. The probe must never wait forever on
// a broken sandbox-exec installation.
func PrepareCommandContext(ctx context.Context, s *Sandbox, command string) (string, error) {
	if s == nil {
		return command, nil
	}
	if err := s.CommandPolicy(command); err != nil {
		return "", err
	}
	// Apply configured resource limits only after validating the original
	// command. Prefixing before CommandPolicy would hide anchored destructive
	// command patterns behind `ulimit … &&`.
	if prefix := s.Prefix(); prefix != "" {
		command = prefix + " " + command
	}
	if s.CurrentMode() != ModeStrict {
		return command, nil
	}
	profile, err := s.Profile()
	if err != nil {
		return "", err
	}
	// Compile the profile before returning a command. sandbox-exec otherwise
	// reports a malformed profile as an ordinary child exit code, which would
	// look like a command failure rather than an unavailable security backend.
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, "/usr/bin/sandbox-exec", "-p", profile, "--", "/bin/sh", "-c", "true")
	if out, err := probe.CombinedOutput(); err != nil {
		return "", fmt.Errorf("sandbox: strict profile could not be loaded: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return "sandbox-exec -p " + shellQuote(profile) + " -- /bin/sh -c " + shellQuote(command), nil
}

// WrapCommand returns a sandbox-exec wrapped command line when strict mode is
// active on macOS and the sandbox-exec utility exists; "" otherwise. The whole
// command line — including any ulimit prefix — is handed to a /bin/sh -c inside
// the sandboxed process, so shell operators (`;`, `&&`, …) cannot split
// execution into a part that runs outside the profile. The profile confines
// file writes to the workspace, allows reads broadly, and denies network
// access unless AllowNetwork is explicitly enabled.
func (s *Sandbox) WrapCommand(cmdline string) string {
	if s == nil || s.CurrentMode() != ModeStrict || cmdline == "" {
		return ""
	}
	profile, err := s.Profile()
	if err != nil {
		// Deprecated callers historically interpreted an empty string as “do
		// not wrap” and then ran cmdline directly. Return a command that fails
		// closed instead, so an unavailable strict backend can never silently
		// become an unsandboxed invocation.
		return "/usr/bin/false # ccdp: strict sandbox unavailable"
	}
	// Do not preflight here: this legacy string API cannot return the probe
	// error. The returned sandbox-exec command still fails closed if the host
	// refuses to apply the profile; callers with an error channel must use
	// PrepareCommand/WrapCommandChecked.
	return "sandbox-exec -p " + shellQuote(profile) + " -- /bin/sh -c " + shellQuote(cmdline)
}

// WrapCommandChecked is the migration-safe form of WrapCommand. New code
// should prefer PrepareCommand directly so backend errors remain actionable.
func (s *Sandbox) WrapCommandChecked(cmdline string) (string, error) {
	return PrepareCommand(s, cmdline)
}

// shellQuote single-quotes a string for safe shell embedding.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// interactivePatterns match commands that require an interactive terminal and
// cannot be driven by the agent non-interactively.
var interactivePatterns = []struct {
	re   *regexp.Regexp
	desc string
}{
	{regexp.MustCompile(`(?i)\b(vim|vimdiff|nvim|vi|nano|emacs|pico|less)\b`), "interactive editor/pager"},
	// Match the shell `read` builtin as a command token, not git plumbing such
	// as `git read-tree` (where the hyphen is part of the subcommand).
	{regexp.MustCompile(`(?i)(^|[\s;&|])read(?:[\s;&|]|$)`), "interactive input"},
	{regexp.MustCompile(`(?i)\b(top|htop|btop)\b`), "interactive process monitor"},
	{regexp.MustCompile(`git\s+(rebase\s+-i|add\s+-p|commit\s+--amend\s+-i|clean\s+-i)`), "interactive git"},
	{regexp.MustCompile(`(?i)\b(ssh)\b\s.*-t\b`), "interactive ssh"},
}

// CheckInteractive reports whether a shell command needs a TTY and cannot be
// run non-interactively. Applies in every sandbox mode.
func CheckInteractive(command string) error {
	c := strings.TrimSpace(command)
	if c == "" {
		return nil
	}
	// Interactive shells/editors run under their own process; bare invocations
	// would hang the tool waiting for input.
	for _, p := range interactivePatterns {
		if p.re.MatchString(c) {
			return fmt.Errorf("sandbox: command %q needs an interactive terminal (%s); run it yourself instead", c, p.desc)
		}
	}
	return nil
}

// networkPatterns match obvious network clients. Strict mode blocks them
// unless AllowNetwork is set.
var networkPatterns = []struct {
	re   *regexp.Regexp
	desc string
}{
	{regexp.MustCompile(`(?i)\bcurl\b`), "curl (network)"},
	{regexp.MustCompile(`(?i)\bwget\b`), "wget (network)"},
	{regexp.MustCompile(`(?i)\b(ssh|scp|sftp)\b`), "ssh/scp/sftp (network)"},
	{regexp.MustCompile(`(?i)\b(nc|netcat|telnet|ftp)\b`), "netcat/telnet/ftp (network)"},
	{regexp.MustCompile(`(?i)\bgit\s+(fetch|pull|clone|push|remote)\b`), "git remote (network)"},
	{regexp.MustCompile(`(?i)\bnpm\s+(install|i|add|update)\b`), "npm install (network)"},
	{regexp.MustCompile(`(?i)\b(pip|pip3)\s+install\b`), "pip install (network)"},
	{regexp.MustCompile(`(?i)\bgo\s+get\b`), "go get (network)"},
	{regexp.MustCompile(`(?i)\bcargo\s+(install|add|update)\b`), "cargo (network)"},
}

// VerifyDir ensures the workspace exists and is a directory.
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
