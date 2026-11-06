package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"strings"
	"sync"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/protocol"
	"ccdp/internal/runtimepath"
	"ccdp/internal/sandbox"
)

// Interactive process tools let the agent drive long-running or interactive
// subprocesses (REPLs, dev servers, test watchers) across multiple tool calls.

// maxProcOutput caps the buffered output per process (old lines are dropped).
const maxProcOutput = 64 * 1024
const maxProcessLog = 32 << 20

// Keep recent completed handles readable, without retaining an unbounded
// collection of output buffers for the lifetime of the session.
const maxCompletedProcesses = 32

// managedProcess is one running subprocess with its stdin pipe and a bounded
// output buffer.
type managedProcess struct {
	mu         sync.Mutex
	identity   protocol.ProcessState
	events     func(protocol.ProcessEvent) (string, error)
	persistErr error
	log        *os.File
	logPath    string
	logSize    int64
	total      int64
	logErr     error
	reason     string
	tty        bool
	cmd        *execution.Process
	tail       processBuffer
	exited     bool
	finishedAt time.Time
	err        error
	done       chan struct{}
}

// ProcessManager owns commands for one Resources owner. Opaque handles bind
// both executor generation and owner, and never alias after runtime restart.
type ProcessManager struct {
	mu         sync.Mutex
	owner      string
	logDir     string
	scope      string
	generation string
	starting   int
	starts     sync.WaitGroup
	finishes   sync.WaitGroup
	procs      map[string]*managedProcess
	closed     bool
	revoking   bool
	closeErr   error
	closeDone  chan struct{}
}

// NewProcessManager creates an empty owner-scoped process manager.
func NewProcessManager(owner, generation, scope string) *ProcessManager {
	return &ProcessManager{owner: owner, generation: generation, scope: scope, procs: map[string]*managedProcess{}, closeDone: make(chan struct{})}
}
func (m *ProcessManager) newID() (string, error) {
	nonce, err := runtimepath.ID()
	if err != nil {
		return "", err
	}
	return "p." + m.generation + "." + m.scope + "." + nonce, nil
}

// Owner returns the immutable scope identifier.
func (m *ProcessManager) Owner() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owner
}

// CheckOpen verifies that the manager can still admit a process operation.
func (m *ProcessManager) CheckOpen() error {
	if m == nil {
		return fmt.Errorf("process: nil manager")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("process manager for %q is closed", m.owner)
	}
	if m.revoking {
		return fmt.Errorf("process manager for %q is stopping processes for sandbox revocation", m.owner)
	}
	return nil
}

type commandTracking struct {
	info   protocol.ProcessInfo
	events func(protocol.ProcessEvent) (string, error)
}

func (mp *managedProcess) fact(kind string) error {
	if mp.events == nil {
		return nil
	}
	mp.mu.Lock()
	state := *protocol.CloneProcessState(&mp.identity)
	mp.mu.Unlock()
	if kind == "finished" {
		state.ProcessOutput = mp.completion()
	}
	outputID, err := mp.events(protocol.ProcessEvent{Kind: kind, State: state})
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if outputID != "" {
		mp.identity.OutputID = outputID
	}
	if err != nil {
		mp.persistErr = err
	}
	return err
}

func (m *ProcessManager) startManaged(ctx context.Context, command, dir string, policy *sandbox.Sandbox, tty, pipe bool, timeout time.Duration, tracking *commandTracking) (string, *managedProcess, error) {
	if err := m.CheckOpen(); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	if m.closed || m.revoking {
		m.mu.Unlock()
		return "", nil, fmt.Errorf("process owner closed or revoking")
	}
	active := m.starting
	for _, p := range m.procs {
		p.mu.Lock()
		if !p.exited {
			active++
		}
		p.mu.Unlock()
	}
	if active >= 32 {
		m.mu.Unlock()
		return "", nil, fmt.Errorf("process limit reached (32 active commands)")
	}
	m.starting++
	m.starts.Add(1)
	logDir := m.logDir
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.starting--; m.mu.Unlock(); m.starts.Done() }()
	if strings.TrimSpace(command) == "" {
		return "", nil, fmt.Errorf("empty command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id, err := m.newID()
	if err != nil {
		return "", nil, err
	}
	if logDir == "" {
		return "", nil, fmt.Errorf("process environment has no private log directory")
	}
	log, err := os.CreateTemp(logDir, "process-*.log")
	if err != nil {
		return "", nil, err
	}
	mp := &managedProcess{done: make(chan struct{}), log: log, logPath: log.Name(), tty: tty,
		identity: protocol.ProcessState{ProcessOutput: protocol.ProcessOutput{ID: id, Status: "registered"}, Revision: 1}}
	if tracking != nil {
		mp.identity.ProcessInfo = tracking.info
		mp.events = tracking.events
	}
	if err = mp.fact("registered"); err != nil {
		log.Close()
		os.Remove(log.Name())
		return "", nil, err
	}
	fail := func(err error) (string, *managedProcess, error) {
		mp.mu.Lock()
		mp.err = err
		mp.exited = true
		mp.reason = "start_failed"
		mp.finishedAt = time.Now().UTC()
		mp.identity.FinishedAt = mp.finishedAt
		mp.identity.Revision++
		mp.mu.Unlock()
		log.Close()
		persistErr := mp.fact("finished")
		close(mp.done)
		mp.release()
		return "", nil, errors.Join(err, persistErr)
	}
	cmd, err := execution.StartShell(ctx, command, dir, execution.SanitizedEnvironmentFor(execution.EnvironmentCommand, os.Environ()), policy)
	if err != nil {
		return fail(err)
	}
	mp.cmd = cmd
	cmd.SetIO(nil, processSink{mp}, processSink{mp})
	if tty {
		err = cmd.SetTerminal(processSink{mp})
	} else if pipe {
		_, err = cmd.StdinPipe()
	}
	cmd.SetTimeout(timeout)
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		execution.CleanupStartedProcess(cmd)
		return fail(err)
	}
	mp.mu.Lock()
	mp.identity.Status = "running"
	mp.identity.StartedAt = time.Now().UTC()
	mp.identity.Revision++
	mp.mu.Unlock()
	startErr := mp.fact("started")
	m.mu.Lock()
	m.procs[id] = mp
	rejected := m.closed || m.revoking
	m.mu.Unlock()
	m.finishes.Add(1)
	go func() {
		defer m.finishes.Done()
		waitErr := cmd.Wait()
		mp.mu.Lock()
		mp.err, mp.exited, mp.finishedAt, mp.reason = waitErr, true, time.Now().UTC(), cmd.Reason()
		mp.identity.FinishedAt = mp.finishedAt
		mp.identity.Revision++
		_ = mp.log.Close()
		mp.mu.Unlock()
		_ = mp.fact("finished")
		close(mp.done)
		m.mu.Lock()
		evicted := m.pruneExitedLocked()
		m.mu.Unlock()
		for _, p := range evicted {
			p.release()
		}
	}()
	if startErr != nil || rejected {
		_, _, stopErr := mp.stop()
		m.Remove(id)
		if rejected {
			startErr = errors.Join(startErr, fmt.Errorf("process owner closed or revoking"))
		}
		return "", nil, errors.Join(startErr, stopErr)
	}
	return id, mp, nil
}

type processSink struct{ p *managedProcess }

func (w processSink) Write(b []byte) (int, error) { w.p.appendOutput(b); return len(b), nil }

// StopAll stops and joins every process currently owned by this manager while
// keeping the manager open for calls authorized after revocation completes.
// Starts remain blocked until ResumeAfterRevocation is called by the owner.
func (m *ProcessManager) StopAll() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("process manager for %q is closed", m.owner)
	}
	m.revoking = true
	m.mu.Unlock()
	m.starts.Wait()
	m.mu.Lock()
	type owned struct {
		id string
		mp *managedProcess
	}
	procs := make([]owned, 0, len(m.procs))
	for id, mp := range m.procs {
		procs = append(procs, owned{id: id, mp: mp})
	}
	m.mu.Unlock()

	var stopErr error
	failed := make(map[string]*managedProcess)
	for _, proc := range procs {
		_, exited, err := proc.mp.stop()
		if err != nil {
			stopErr = errors.Join(stopErr, err)
		}
		if !exited && err == nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("background process did not exit after sandbox revocation"))
		}
		if !exited || err != nil {
			failed[proc.id] = proc.mp
		} else {
			m.Remove(proc.id)
		}
	}
	if len(failed) > 0 {
		m.mu.Lock()
		for id, mp := range failed {
			m.procs[id] = mp
		}
		m.mu.Unlock()
	}
	return stopErr
}

// Count returns the number of session-owned background processes not yet
// confirmed exited.
func (m *ProcessManager) Count() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, mp := range m.procs {
		mp.mu.Lock()
		if !mp.exited {
			count++
		}
		mp.mu.Unlock()
	}
	return count
}

// Caller holds m.mu. Eviction releases both the handle and its temporary log.
func (m *ProcessManager) pruneExitedLocked() []*managedProcess {
	var released []*managedProcess
	type completed struct {
		id string
		at time.Time
	}
	var exited []completed
	for id, mp := range m.procs {
		mp.mu.Lock()
		done := false
		select {
		case <-mp.done:
			done = true
		default:
		}
		at := mp.finishedAt
		mp.mu.Unlock()
		if done {
			exited = append(exited, completed{id, at})
		}
	}
	sort.Slice(exited, func(i, j int) bool {
		if exited[i].at.Equal(exited[j].at) {
			return exited[i].id < exited[j].id
		}
		return exited[i].at.Before(exited[j].at)
	})
	for _, proc := range exited[:max(0, len(exited)-maxCompletedProcesses)] {
		released = append(released, m.procs[proc.id])
		delete(m.procs, proc.id)
	}
	return released
}

// ResumeAfterRevocation allows new starts after the owner has confirmed that
// every process holding the older sandbox policy has exited.
func (m *ProcessManager) ResumeAfterRevocation() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.closed {
		m.revoking = false
	}
	m.mu.Unlock()
}

func (mp *managedProcess) appendOutput(fragment []byte) {
	if len(fragment) == 0 {
		return
	}
	mp.mu.Lock()
	mp.total += int64(len(fragment))
	if mp.log != nil && mp.logErr == nil && mp.logSize < maxProcessLog {
		n := min(int64(len(fragment)), maxProcessLog-mp.logSize)
		written, err := mp.log.Write(fragment[:n])
		mp.logSize += int64(written)
		mp.logErr = err
	}
	mp.tail.Write(fragment)
	mp.mu.Unlock()
}

func (mp *managedProcess) write(input string) error { return mp.cmd.WriteInput([]byte(input)) }

func (mp *managedProcess) stop() (string, bool, error) {
	if mp == nil {
		return "", true, nil
	}
	if err := mp.cmd.Stop("cancelled"); err != nil {
		return "", false, err
	}
	<-mp.done
	mp.mu.Lock()
	err := mp.persistErr
	mp.mu.Unlock()
	return "", true, err
}

func (mp *managedProcess) release() {
	mp.removeLog()
	mp.mu.Lock()
	if mp.identity.Released {
		mp.mu.Unlock()
		return
	}
	mp.identity.Released = true
	mp.identity.Revision++
	mp.mu.Unlock()
	_ = mp.fact("released")
}

func (mp *managedProcess) removeLog() {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if mp.exited && mp.logPath != "" {
		if err := os.Remove(mp.logPath); err == nil || os.IsNotExist(err) {
			mp.logPath = ""
		}
	}
}

// Close stops every process owned by this manager and rejects future starts.
// It is idempotent; all calls return the same first close result.
func (m *ProcessManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		m.mu.Unlock()
		<-done
		m.mu.Lock()
		err := m.closeErr
		m.mu.Unlock()
		return err
	}
	m.closed = true
	m.mu.Unlock()
	m.starts.Wait()
	m.mu.Lock()
	procs := make([]*managedProcess, 0, len(m.procs))
	for _, mp := range m.procs {
		procs = append(procs, mp)
	}
	m.mu.Unlock()
	var closeErr error
	for _, mp := range procs {
		// A signal/exit status from an intentionally stopped background
		// process is not a Close failure. Close's contract is resource
		// reclamation; the process result is available through Process.
		_, exited, err := mp.stop()
		closeErr = errors.Join(closeErr, err)
		if exited {
			mp.release()
		}
	}
	m.mu.Lock()
	for id, mp := range m.procs {
		mp.mu.Lock()
		exited := mp.exited
		mp.mu.Unlock()
		if exited {
			delete(m.procs, id)
		}
	}
	m.mu.Unlock()
	m.finishes.Wait()
	m.mu.Lock()
	m.closeErr = closeErr
	close(m.closeDone)
	m.mu.Unlock()
	return closeErr
}

func (m *ProcessManager) belongs(pid string) bool {
	return strings.HasPrefix(pid, "p."+m.generation+"."+m.scope+".")
}

// Get returns a live process by a handle created by this manager.
func (m *ProcessManager) Get(pid string) (*managedProcess, error) {
	if m == nil {
		return nil, fmt.Errorf("process: nil manager")
	}
	if !strings.HasPrefix(pid, "p."+m.generation+".") {
		return nil, fmt.Errorf("stale_process: handle belongs to a different runtime; do not retry")
	}
	if !m.belongs(pid) {
		return nil, fmt.Errorf("foreign_process: handle belongs to another owner")
	}
	m.mu.Lock()
	mp, ok := m.procs[pid]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("process_released: handle %s is no longer available", pid)
	}
	return mp, nil
}

// Remove forgets a stopped process handle.
func (m *ProcessManager) Remove(pid string) {
	m.mu.Lock()
	mp := m.procs[pid]
	if mp != nil {
		select {
		case <-mp.done:
			delete(m.procs, pid)
		default:
			mp = nil
		}
	}
	m.mu.Unlock()
	if mp != nil {
		mp.release()
	}
}
