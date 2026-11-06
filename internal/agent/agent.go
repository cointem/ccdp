package agent

import (
	"context"
	cryptorand "crypto/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/sessionview"
)

// Agent composes session services and the command owner. Business transitions
// enter through typed owner messages; construction initializes these values
// exclusively before publication. mu protects read views and producer inboxes.
type Agent struct {
	persistenceFault   atomic.Pointer[error]
	publishedSnapshot  atomic.Pointer[protocol.SessionView]
	snapshotProjection snapshotProjection
	services           sessionServices
	owner              *sessionOwner
	conversation       conversationState
	lifecycle          sessionLifecycle
	turn               turnLifecycleData
	decisions          approvalState
	capabilities       capabilityState
	discovery          toolDiscoveryData
	modelsState        modelState
	budget             budgetState
	turnMemory         turnMemoryState
	sequences          seqState
	inbox              dedupState
	publications       watchState
	children           childRuntimeBindings

	constructionComplete     bool   // immutable once the constructor returns
	configuredModel          string // launch model, independent of the active selection
	catalogNotificationsDone <-chan struct{}
	memorySessions           *memorySessionCatalog
	memoryHistoryDone        chan struct{}
	memoryHistoryCancel      context.CancelFunc
	memoryHistoryAutomatic   bool
	memoryHistoryRunning     bool
	memoryHistoryAttempt     time.Time
	finalization             *childFinalization // admission shares the close boundary under mu
	streamEpoch              string
	transcript               *sessionview.Transcript
	views                    *sessionview.Hub // owner access only

	// Readers observe config, catalog and policy from the same published generation.
	settingsCommitMu sync.RWMutex
	mu               sync.Mutex

	toolUses            int
	toolsTokensEstimate int
	publishedCatalog    struct{ catalog, settings, tools uint64 } // owner access only
	historyTokenCache   struct {
		first  *messages.Message
		prefix []int
	}
	trace   *os.File
	traceMu sync.Mutex
}

// AddDirectory grants the sandbox access to an extra directory at runtime.
func (a *Agent) AddDirectory(dir string) { a.setDirectoryAccess(dir, false) }

// AddDisallowedDirectory blocks a directory at runtime.
func (a *Agent) AddDisallowedDirectory(dir string) { a.setDirectoryAccess(dir, true) }

// syncHookContext keeps hook payloads and execution policy aligned with the
// published agent state after mode, workspace, sandbox or session changes.
func (a *Agent) syncHookContext() {
	a.mu.Lock()
	sessionID, ws := a.conversation.sessionID, a.services.cfg.Workspace
	policy := a.services.sandbox.Snapshot()
	a.mu.Unlock()
	a.services.hooks.SetSandbox(policy)
	a.services.hooks.SetContext(sessionID, ws, string(a.services.perms.CurrentMode()))
}

// Usage returns a snapshot of the session's token accounting.
func (a *Agent) Usage() protocol.UsageSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runtimeUsageLocked()
}

// ToolUses returns how many tool calls reached execution this session.
func (a *Agent) ToolUses() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.toolUses
}

// SessionID returns the session identifier.
func (a *Agent) SessionID() string { return a.conversation.sessionID }

// WorkspaceLabel returns the absolute workspace path.
func (a *Agent) WorkspaceLabel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services.cfg.Workspace
}

// SessionDir returns the directory where sessions are persisted.
func (a *Agent) SessionDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services.cfg.SessionDir
}

// currentSandbox returns the sandbox in use. The pointer is swapped by
// SetWorkspace (UI goroutine), so tools and turn goroutines must not read the
// field directly.
func (a *Agent) currentSandbox() *sandbox.Sandbox {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.services.sandbox
}

func newSessionID() string {
	// Session IDs are also directory names and can be generated concurrently
	// by independent processes. A cryptographically random base32 value avoids
	// the microsecond timestamp collisions of the old format without adding a
	// process-local counter that would still collide across processes.
	return cryptorand.Text()
}
