package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"ccdp/internal/computer"
	"ccdp/internal/execution"
	"ccdp/internal/runtimepath"
	"ccdp/internal/sandbox"
)

// Resources owns the mutable state that a tool invocation may carry between
// calls.  A Resources value is deliberately session-scoped: it is created by
// the runtime when a session is opened and closed with that session.  Tools
// must receive the same value in Context for the lifetime of a session.
type Resources struct {
	owner          string
	sessionDir     string
	ownerCtx       context.Context
	ownerCancel    context.CancelFunc
	scratchDir     string
	environmentDir string
	ownerToken     string
	initErr        error

	// Exactly one owner-owned instance of each mutable session resource.
	Processes *ProcessManager
	Files     *FileState
	// The controller is owned only by the root session; children inherit its ID.
	executor        *execution.Controller
	executorID      string
	fileWorkersOnce sync.Once
	fileWorkers     chan *fileWorker
	computer        *computer.Controller

	mu       sync.RWMutex
	closed   bool
	closeErr error
	once     sync.Once
}

// NewResources creates an owner-scoped resource container. The session ID is
// an identity boundary and sessionDir is the durable directory for retained
// tool artifacts. Both values are explicit to prevent accidental reuse
// of a parent/root directory as a session scope.
func NewResources(owner, sessionDir string, runtimeRoot ...string) *Resources {
	return NewResourcesWithContext(owner, sessionDir, context.Background(), "", runtimeRoot...)
}

// NewResourcesWithContext creates an owner-scoped resource container whose
// long-lived process lifetime is bound to ownerCtx. The context is captured at
// construction and cannot be replaced while resources are in use; Close also
// cancels the derived context before reclaiming individual resources. An empty
// executorID creates a root controller; child owners inherit their root ID.
func NewResourcesWithContext(owner, sessionDir string, ownerCtx context.Context, executorID string, runtimeRoot ...string) *Resources {
	if owner == "" {
		owner = "session"
	}
	if ownerCtx == nil {
		ownerCtx = context.Background()
	}
	ownerCtx, ownerCancel := context.WithCancel(ownerCtx)
	r := &Resources{owner: owner, sessionDir: sessionDir, ownerCtx: ownerCtx, ownerCancel: ownerCancel}
	if executorID == "" {
		r.executor = execution.NewController(runtimeRoot...)
		executorID = r.executor.ID
	}
	r.executorID = executorID
	r.ownerToken, r.initErr = runtimepath.ID()
	if r.initErr == nil {
		r.environmentDir, r.initErr = execution.NewEnvironment(executorID)
	}
	if r.initErr == nil {
		r.scratchDir = filepath.Join(r.environmentDir, "scratch")
	}
	r.Processes = NewProcessManager(owner, executorID, r.ownerToken)
	if r.initErr == nil {
		r.Processes.logDir = filepath.Join(r.environmentDir, "logs")
	}
	r.Files = NewFileState(owner)
	return r
}

// ExecutorID binds this owner to its root session tree's execution service.
func (r *Resources) ExecutorID() string { return r.executorID }

// Computer is injected only into an explicitly enabled root invocation.
func (r *Resources) Computer() *computer.Controller {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if r.computer == nil {
		r.computer = computer.NewController()
	}
	return r.computer
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

// ReplaceScratch installs a prepared environment after all managed work has
// joined. The old directory may still be used by unconfirmed descendants and
// is deliberately retained; existing process output handles stay readable.
func (r *Resources) ReplaceScratch(path string) {
	r.mu.Lock()
	r.scratchDir = path
	r.environmentDir = filepath.Dir(path)
	r.mu.Unlock()
	r.Processes.mu.Lock()
	r.Processes.logDir = filepath.Join(filepath.Dir(path), "logs")
	r.Processes.mu.Unlock()
}

func (r *Resources) NewScratch() (string, error) {
	root, err := execution.NewEnvironment(r.executorID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "scratch"), nil
}

func (r *Resources) BindSandbox(sb *sandbox.Sandbox) {
	if sb == nil {
		return
	}
	sb.ExecutorID = r.executorID
	execution.BindPrivateRoots(sb)
	sb.AddScratchDir(r.ScratchDir())
	if dir := r.SessionDir(); dir != "" {
		sb.AddArtifactRoot(filepath.Join(dir, "outputs"))
	}
	r.Processes.mu.Lock()
	logs := r.Processes.logDir
	r.Processes.mu.Unlock()
	if logs != "" {
		sb.AddArtifactRoot(logs)
		sb.AddReadOnlyDir(logs)
	}
	execution.BindDevelopmentCache(sb)
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
	if r.initErr != nil {
		return fmt.Errorf("execution resources: %w", r.initErr)
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
		desktop := r.computer
		r.mu.Unlock()
		if ownerCancel != nil {
			ownerCancel()
		}
		if desktop != nil {
			desktop.Close()
		}
		var closeErr error
		if r.Processes != nil {
			closeErr = r.Processes.Close()
		}
		if r.Files != nil {
			r.Files.Close()
		}
		r.closeFileWorkers()
		if r.executor != nil {
			if err := r.executor.Close(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
		if r.environmentDir != "" && closeErr == nil {
			if err := os.RemoveAll(r.environmentDir); err != nil && closeErr == nil {
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
// may further set Args, Timeout and Sandbox on the returned value.
func (r *Resources) Context(parent context.Context, workingDir string, sb *sandbox.Sandbox) *Context {
	if parent == nil {
		parent = context.Background()
	}
	if sb != nil {
		sb = sb.Snapshot()
		r.BindSandbox(sb)
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
