package tools

import (
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/skills"
	"context"

	"fmt"

	"path/filepath"

	"strings"

	"time"
)

// SpawnAgentRequest is one model-facing assignment; lifecycle policy stays in the runtime.
type SpawnAgentRequest struct {
	Task      string
	Name      string
	Role      string
	Workspace string
	Context   string
}

// Context carries per-invocation state into a tool.
type Context struct {
	dataResult *Result
	RunCode    func(string, time.Duration) (protocol.CodeResult, error)
	context.Context
	ReadRoot   string         // optional strict snapshot-only read boundary
	WorkingDir string         // directory the tool should operate in
	SessionDir string         // session directory for retained tool artifacts
	Args       map[string]any // parsed JSON arguments from the model
	Timeout    time.Duration  // per-tool timeout (0 = default)
	// Resources owns the session's file state and process handles.
	Resources *Resources
	// ReplaceTodos commits a validated task replacement through the session owner.
	// It must publish the new list only after its durable commit succeeds.
	ReplaceTodos func([]TodoState) error
	// Goals is scoped by the runtime to this invocation's session and goal.
	// Child adapters may observe the parent's goal but cannot mutate it.
	Goals GoalControl
	// Owner is the resource scope expected by this invocation.  A non-empty
	// value is checked against Resources.Owner so a stale/cross-session context
	// cannot accidentally use another session's handles.
	Owner string
	// ReadLimit and OutputLimit bound bytes admitted from the filesystem and
	// bytes returned/retained by a tool. Zero uses package defaults.
	ReadLimit   int
	OutputLimit int
	// ProcessInputLimit bounds one ProcessWrite call. Zero uses the default.
	ProcessInputLimit int
	// Sandbox applies the shared file policy: host reads are allowed except
	// explicit denies, while writes require an authorized root.
	Sandbox *sandbox.Sandbox
	// HostNetworkAllowed authorizes this invocation's in-process network
	// adapters. Seatbelt does not constrain the harness process itself.
	HostNetworkAllowed bool
	// SpawnAgent admits one independent assignment and returns its stable identity.
	SpawnAgent       func(SpawnAgentRequest) (protocol.AgentReceipt, error)
	Sessions         protocol.SessionDirectory
	AgentCommandID   protocol.CommandID
	SendAgentMessage func(target, text string) error
	WaitAgentEvent   func(context.Context) (string, error)
	ReadAgent        func(context.Context, protocol.AgentReadRequest, int) (string, error)
	ReadOutput       func(context.Context, string, int64, int) (protocol.OutputPage, error)
	// Skills exposes the loaded skill store for ReadSkill lookups.
	Skills skills.Provider
	// BeforeWrite durably captures a directly attributed mutation. The returned
	// finalizer records a successful mutation; failed/uncertain publication is
	// left incomplete rather than attributing another writer's bytes to us.
	// knownBefore is supplied by Edit to reuse its already-read original bytes;
	// nil means the recorder must capture the old file itself (Write).
	BeforeWrite      func(path string, knownBefore []byte) (func() error, error)
	NavigateSemantic func(operation, path string, line, character int) (string, error)
}

// WithArgs gives an invocation its own context fields while retaining the
// explicitly shared session resources. Arguments are prepared by the registry.
func (c *Context) WithArgs(args map[string]any) *Context {
	if c == nil {
		return &Context{Args: args}
	}
	local := *c
	local.Args = args
	return &local
}

const (
	// DefaultReadLimit is the maximum bytes one file read admits by default.
	DefaultReadLimit = 512 * 1024
	// DefaultOutputLimit bounds model-visible tool output. Raw process output is
	// bounded independently by ProcessManager.
	DefaultOutputLimit = 512 * 1024
	// DefaultProcessInputLimit bounds one write sent to a background process.
	DefaultProcessInputLimit = 64 * 1024
)

func (c *Context) readLimit() int {
	if c != nil && c.ReadLimit > 0 {
		return c.ReadLimit
	}
	return DefaultReadLimit
}

func (c *Context) outputLimit() int {
	if c != nil && c.OutputLimit > 0 {
		return c.OutputLimit
	}
	return DefaultOutputLimit
}

func (c *Context) processInputLimit() int {
	if c != nil && c.ProcessInputLimit > 0 {
		return c.ProcessInputLimit
	}
	return DefaultProcessInputLimit
}

// checkResources verifies that the invocation is still attached to its
// session owner. Runtime code can call this before dispatching a tool; the
// built-in tools also call their specific owner accessors.
func (c *Context) checkResources() error {
	if c == nil {
		return fmt.Errorf("tools: nil context")
	}
	if c.Context != nil {
		if err := c.Context.Err(); err != nil {
			return err
		}
	}
	if c.Resources == nil {
		return nil
	}
	if err := c.Resources.CheckOpen(); err != nil {
		return err
	}
	if c.Owner != "" && c.Owner != c.Resources.Owner() {
		return fmt.Errorf("tools: context owner %q does not match resource owner %q", c.Owner, c.Resources.Owner())
	}
	return nil
}

func (c *Context) fileState() *FileState {
	if c != nil && c.Resources != nil {
		return c.Resources.Files
	}
	return nil
}

func (c *Context) fileStateForUse() (*FileState, error) {
	state := c.fileState()
	if state == nil {
		return nil, fmt.Errorf("tools: no session FileState injected")
	}
	if err := state.CheckOpen(); err != nil {
		return nil, err
	}
	return state, nil
}

func (c *Context) processManager() (*ProcessManager, error) {
	if err := c.checkResources(); err != nil {
		return nil, err
	}
	if c.Resources != nil {
		if c.Resources.Processes == nil {
			return nil, fmt.Errorf("tools: resource owner %q has no process manager", c.Resources.Owner())
		}
		return c.Resources.Processes, nil
	}
	return nil, fmt.Errorf("tools: no session ProcessManager injected")
}

// ResolveRead resolves p for reading. A missing policy is a denied operation.
func (c *Context) ResolveRead(p string) (string, error) {
	if err := c.checkResources(); err != nil {
		return "", err
	}
	if c.Sandbox == nil {
		return "", fmt.Errorf("tools: sandbox policy is unavailable")
	}
	resolved, err := c.Sandbox.ResolveRead(p)
	if err != nil {
		return "", err
	}
	if c.ReadRoot != "" {
		root, e := filepath.EvalSymlinks(c.ReadRoot)
		if e != nil {
			return "", e
		}
		target, e := filepath.EvalSymlinks(resolved)
		if e != nil {
			return "", e
		}
		rel, e := filepath.Rel(root, target)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("read outside snapshot")
		}
	}
	return resolved, nil
}

// ResolveWrite resolves p for writing. A missing policy is a denied operation.
func (c *Context) ResolveWrite(p string) (string, error) {
	if err := c.checkResources(); err != nil {
		return "", err
	}
	if c.Sandbox == nil {
		return "", fmt.Errorf("tools: sandbox policy is unavailable")
	}
	return c.Sandbox.ResolveWrite(p)
}
