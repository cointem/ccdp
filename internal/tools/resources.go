package tools

import (
	"context"
	"fmt"
	"os"
	"sync"

	"ccdp/internal/sandbox"
)

// Resources owns the mutable state that a tool invocation may carry between
// calls.  A Resources value is deliberately session-scoped: it is created by
// the runtime when a session is opened and closed with that session.  Tools
// must receive the same value in Context for the lifetime of a session.
type Resources struct {
	owner       string
	sessionDir  string
	ownerCtx    context.Context
	ownerCancel context.CancelFunc
	scratchDir  string

	// Exactly one owner-owned instance of each mutable session resource.
	Processes *ProcessManager
	Todos     *TodoStore
	Files     *FileState

	mu       sync.RWMutex
	closed   bool
	closeErr error
	once     sync.Once
}

// NewResources creates an owner-scoped resource container. The session ID is
// an identity boundary and sessionDir is the durable directory for scratch
// state (todos, etc.). Both values are explicit to prevent accidental reuse
// of a parent/root directory as a session scope.
func NewResources(owner, sessionDir string) *Resources {
	return NewResourcesWithContext(owner, sessionDir, context.Background())
}

// NewResourcesWithContext creates an owner-scoped resource container whose
// long-lived process lifetime is bound to ownerCtx. The context is captured at
// construction and cannot be replaced while resources are in use; Close also
// cancels the derived context before reclaiming individual resources.
func NewResourcesWithContext(owner, sessionDir string, ownerCtx context.Context) *Resources {
	if owner == "" {
		owner = "session"
	}
	if ownerCtx == nil {
		ownerCtx = context.Background()
	}
	ownerCtx, ownerCancel := context.WithCancel(ownerCtx)
	scratch, _ := os.MkdirTemp("", "ccdp-session-")
	r := &Resources{owner: owner, sessionDir: sessionDir, ownerCtx: ownerCtx, ownerCancel: ownerCancel, scratchDir: scratch}
	r.Processes = NewProcessManager(owner)
	r.Todos = NewTodoStore(owner, sessionDir)
	r.Files = NewFileState(owner)
	return r
}

// ScratchDir returns the private, session-scoped execution scratch root. It is
// removed after the owner stops processes and closes its resources.
func (r *Resources) ScratchDir() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scratchDir
}

// Owner returns the immutable scope identifier carried by this container.
func (r *Resources) Owner() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.owner
}

// SessionDir returns the durable scratch directory associated with the
// resource owner.
func (r *Resources) SessionDir() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessionDir
}

// OwnerContext returns the immutable lifetime context for resources owned by
// this session. Per-invocation contexts must not be used to start long-lived
// processes; Close cancels this context and then tears down the manager.
func (r *Resources) OwnerContext() context.Context {
	if r == nil {
		return context.Background()
	}
	r.mu.RLock()
	ctx := r.ownerCtx
	r.mu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// Closed reports whether this resource owner has been closed.
func (r *Resources) Closed() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.closed
}

// CheckOpen makes stale contexts fail before they can use a resource from a
// closed session.  It is intentionally small so runtime/tooling can use it at
// admission boundaries as well as individual tools.
func (r *Resources) CheckOpen() error {
	if r == nil {
		return fmt.Errorf("tools: missing session resources")
	}
	if r.Closed() {
		return fmt.Errorf("tools: session resources for %q are closed", r.Owner())
	}
	return nil
}

// Close releases every resource owned by this session.  It is idempotent and
// does not inspect or stop resources belonging to another Resources value.
func (r *Resources) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		ownerCancel := r.ownerCancel
		r.mu.Unlock()
		if ownerCancel != nil {
			ownerCancel()
		}
		var closeErr error
		if r.Processes != nil {
			closeErr = r.Processes.Close()
		}
		if r.Todos != nil {
			r.Todos.Close()
		}
		if r.Files != nil {
			r.Files.Close()
		}
		if r.scratchDir != "" {
			if err := os.RemoveAll(r.scratchDir); err != nil && closeErr == nil {
				closeErr = fmt.Errorf("remove session execution scratch: %w", err)
			}
		}
		r.mu.Lock()
		r.closeErr = closeErr
		r.mu.Unlock()
	})
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.closeErr
}

// Context returns a tool context bound to this resource owner.  Runtime code
// may further set Args, Timeout, Sandbox and Notify on the returned value.
func (r *Resources) Context(parent context.Context, workingDir string, sb *sandbox.Sandbox) *Context {
	if parent == nil {
		parent = context.Background()
	}
	return &Context{
		Context:    parent,
		WorkingDir: workingDir,
		SessionDir: r.SessionDir(),
		Resources:  r,
		Sandbox:    sb,
		Owner:      r.Owner(),
		Args:       map[string]any{},
	}
}

// BindResources returns a copy of c bound to r.  It is useful when a runtime
// has a common base context and wants an invocation-specific argument map.
func BindResources(c *Context, r *Resources) *Context {
	if c == nil {
		c = &Context{}
	}
	copyCtx := *c
	copyCtx.Resources = r
	if r != nil {
		copyCtx.Owner = r.Owner()
		if copyCtx.SessionDir == "" {
			copyCtx.SessionDir = r.SessionDir()
		}
	}
	return &copyCtx
}
