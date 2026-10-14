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
	context.Context
	ReadRoot   string         // optional strict snapshot-only read boundary
	WorkingDir string         // directory the tool should operate in
	SessionDir string         // directory where the session persists (todos, etc.)
	Args       map[string]any // parsed JSON arguments from the model
	Timeout    time.Duration  // per-tool timeout (0 = default)
	// Resources is the session-owned resource container. Runtime code must set
	// this field for stateful tools; narrow capability fields below are only for
	// explicit adapters that own the corresponding resource themselves.
	Resources *Resources
	// ProcessManager is an optional narrow injection for callers that only
	// provide process capabilities.  When Resources is non-nil its manager
	// takes precedence and this field is ignored.
	ProcessManager *ProcessManager
	// FileState and TodoStore are optional narrow injections.  They are useful
	// to adapters migrating one tool at a time; Resources remains preferred.
	FileState *FileState
	TodoStore *TodoStore
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
	// Notify is called with transient progress updates (e.g. command output)
	// that the UI can stream live for long-running tools.
	Notify func(line string)
	// SpawnAgent admits one independent assignment and returns its stable identity.
	SpawnAgent       func(SpawnAgentRequest) (protocol.AgentReceipt, error)
	Sessions         protocol.SessionDirectory
	AgentCommandID   protocol.CommandID
	SendAgentMessage func(target, text string) error
	WaitAgentEvent   func(context.Context) (string, error)
	ReadAgent        func(context.Context, protocol.AgentReadRequest, int) (string, error)
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

// notifyContext adapts the runtime's short, synchronous event callback to the
// execution package's cancellable observer contract. Runtime callbacks should
// publish to their bounded event bus and return promptly; callers that may
// block should provide execution.Request.Progress instead.
func (c *Context) notifyContext() func(context.Context, string) error {
	if c == nil || c.Notify == nil {
		return nil
	}
	return func(ctx context.Context, line string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		c.Notify(line)
		return nil
	}
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

func (c *Context) todoStore() *TodoStore {
	if c != nil && c.Resources != nil {
		return c.Resources.Todos
	}
	if c != nil && c.TodoStore != nil {
		return c.TodoStore
	}
	return nil
}

func (c *Context) todoStoreForUse() (*TodoStore, error) {
	store := c.todoStore()
	if store == nil {
		return nil, fmt.Errorf("tools: no session TodoStore injected")
	}
	if err := store.CheckOpen(); err != nil {
		return nil, err
	}
	return store, nil
}

func (c *Context) fileState() *FileState {
	if c != nil && c.Resources != nil {
		return c.Resources.Files
	}
	if c != nil && c.FileState != nil {
		return c.FileState
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
	if c.ProcessManager != nil {
		if err := c.ProcessManager.CheckOpen(); err != nil {
			return nil, err
		}
		return c.ProcessManager, nil
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
