package agent

import (
	"context"
	"sync"

	"ccdp/internal/atomicfile"
	"ccdp/internal/config"
	"ccdp/internal/mcp"
	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
	"ccdp/internal/supervision"
	"ccdp/internal/tools"
)

// SessionSupervisor adapts application resources to the independent run directory.
// Each session owner writes its own log. Cross-owner waits release run and
// directory locks; publicationMu only orders short admission/view transitions.
type SessionSupervisor struct {
	directory     supervision.Supervisor[*managedRun]
	publicationMu sync.Mutex // orders admission and child view publication, never held over I/O
	root          supervisorRoot
	sessionDir    string
	recoveryErr   error
	recoverOnce   sync.Once
}

type managedRun struct {
	handoff              chan struct{} // previous run mailbox waits for continuation admission
	commandsInFlight     int
	commandChanges       chan struct{}
	reviewOutcome        protocol.ExecutionOutcome
	workspaceLease       *atomicfile.Lock
	reviewApprovalReply  chan bool
	reviewCommand        *protocol.Command
	initialInput         *protocol.SubmitInput
	initialCommandID     protocol.CommandID
	forkHistory          []messages.Message
	mu                   sync.Mutex
	fact                 session.ChildRunRecorded
	parent               childParent
	agent                supervision.ChildRuntime
	cancel               context.CancelFunc
	done                 chan struct{}
	cfg                  config.Config
	opts                 Options
	lease                *mcp.StepLease
	toolLease            *tools.Lease
	preHooks             []plugin.PreToolHook
	final                protocol.SessionView
	memory               []session.Record
	memoryBlobs          journal.ArtifactAccess
	err                  error
	executionBoundaryErr error
}

func newSessionSupervisor(root *Agent) *SessionSupervisor {
	s := &SessionSupervisor{root: root, sessionDir: root.services.cfg.SessionDir}
	firstSeen := make(map[protocol.RunID]session.Cursor)
	currentRun := make(map[protocol.SessionID]session.Cursor)
	if p := root.persistenceHandle(); p != nil {
		if records, err := p.Read(session.Beginning); err == nil {
			for _, record := range records {
				if fact, ok := record.Event.(*session.ChildRunRecorded); ok && fact.Child.ParentSessionID == protocol.SessionID(root.conversation.sessionID) {
					copy := *fact
					if copy.Child.Run.Active() {
						copy.Child.Run.Status = "unknown"
						copy.Child.Run.Error = "previous process ended before this run settled"
					}
					done := make(chan struct{})
					close(done)
					r := &managedRun{fact: copy, parent: root, done: done}
					if firstSeen[copy.Child.Run.ID] == 0 {
						firstSeen[copy.Child.Run.ID] = record.Seq
					}
					// A delayed delivery receipt for an older run must not move
					// the session pointer backwards after a continuation was queued.
					if firstSeen[copy.Child.Run.ID] >= currentRun[copy.Child.SessionID] {
						s.directory.Restore(copy.Child, r, true)
						currentRun[copy.Child.SessionID] = firstSeen[copy.Child.Run.ID]
					}
					s.directory.Restore(copy.Child, r, false)
				}
			}
		} else {
			s.recoveryErr = err
		}
	}
	s.readChildOutboxes()
	for _, run := range s.directory.Children() {
		s.projectOutstandingWork(s.childRowLocked(run))
	}
	return s
}

func (a *Agent) Sessions() protocol.SessionDirectory { return a.services.supervisor }

// SetChildInteraction is an application-owned human input capability. Merely
// opening an observer must never enable approvals.
func (a *Agent) SetChildInteraction(enabled bool) {
	if a.services.supervisor == nil {
		return
	}
	a.services.supervisor.directory.SetInteractive(enabled)
}
