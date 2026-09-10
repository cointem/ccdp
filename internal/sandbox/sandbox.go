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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Mode selects how aggressively the sandbox confines tool access.
type Mode string

const (
	ModeConfine Mode = "confine" // file writes confined to workspace; reads allowed outside
	ModeStrict  Mode = "strict"  // every file access confined to workspace
	ModeNone    Mode = "none"    // no containment (Bash-only workflows)
)

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

	// Limits, when non-zero, are applied to every Bash tool invocation as
	// shell ulimit prefixes (Codex's rlimit enforcement, userspace flavor).
	Limits *Limits

	// AllowNetwork permits outbound network commands in strict mode. When
	// false, strict mode blocks obvious network clients.
	AllowNetwork bool

	mu sync.RWMutex // guards Mode, AdditionalDirs and DisallowedDirs
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

func (s *Sandbox) absDir(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
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
		return filepath.Join(".", p), nil
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
	if s == nil || s.CurrentMode() == ModeNone {
		return s.Resolve(p)
	}
	abs, err := s.Resolve(p)
	if err != nil {
		return "", err
	}
	if !s.InWorkspace(abs) {
		return "", fmt.Errorf("sandbox: write to %s is outside workspace %s", p, s.Workspace)
	}
	return abs, nil
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
	if !s.AllowNetwork {
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

// WrapCommand returns a sandbox-exec wrapped command line when strict mode is
// active on macOS and the sandbox-exec utility exists; "" otherwise. The whole
// command line — including any ulimit prefix — is handed to a /bin/sh -c inside
// the sandboxed process, so shell operators (`;`, `&&`, …) cannot split
// execution into a part that runs outside the profile. The profile confines
// file writes to the workspace, allows reads broadly, permits localhost, and
// denies other network activity.
func (s *Sandbox) WrapCommand(cmdline string) string {
	if s == nil || s.CurrentMode() != ModeStrict || cmdline == "" {
		return ""
	}
	if runtime.GOOS != "darwin" {
		return ""
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		return "" // not available; rely on the userspace checks
	}
	profile := fmt.Sprintf(`(version 1)
(allow default)
(deny network*)
(allow network* (remote ip "localhost:*") (remote unix-socket))
(allow file-read*)
(deny file-write*)
(allow file-write* (subpath %q) (subpath "/private/tmp") (subpath "/tmp"))
(deny process-fork)
(allow process-fork (literal "/bin/sh") (literal "/bin/zsh") (literal "/bin/bash"))`, s.Workspace)
	return "sandbox-exec -p " + shellQuote(profile) + " -- /bin/sh -c " + shellQuote(cmdline)
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
	{regexp.MustCompile(`(?i)\b(read|more)\b`), "interactive input"},
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
