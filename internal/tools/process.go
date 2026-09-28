package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
)

// Interactive process tools let the agent drive long-running or interactive
// subprocesses (REPLs, dev servers, test watchers) across multiple tool calls.

// maxProcOutput caps the buffered output per process (old lines are dropped).
const maxProcOutput = 512 * 1024
const maxProcessLog = 32 << 20

const maxProcessWait = 30 * time.Second

// Keep recent completed handles readable, without retaining an unbounded
// collection of output buffers for the lifetime of the session.
const maxCompletedProcesses = 32

// managedProcess is one running subprocess with its stdin pipe and a bounded
// output buffer.
type managedProcess struct {
	mu         sync.Mutex
	log        *os.File
	logPath    string
	logSize    int64
	total      int64
	logErr     error
	changed    chan struct{}
	reason     string
	tty        bool
	cmd        *execution.Process
	buf        []byte // bounded output buffer
	exited     bool
	finishedAt time.Time
	err        error
	done       chan struct{}
}

// ProcessManager owns all background processes started by one Resources
// owner. Handles include a manager scope tag, so an integer returned by one
// session is rejected by every other manager even if both have a local id 1.
type ProcessManager struct {
	mu        sync.Mutex
	owner     string
	logDir    string
	scope     uint32
	next      uint32
	starting  int
	starts    sync.WaitGroup
	procs     map[int]*managedProcess
	closed    bool
	revoking  bool
	closeErr  error
	closeDone chan struct{}
}

var processScopeSequence atomic.Uint32

// NewProcessManager creates an empty owner-scoped process manager.
func NewProcessManager(owner string) *ProcessManager {
	scope := processScopeSequence.Add(1) & 0x7fffffff
	if scope == 0 {
		scope = 1
	}
	return &ProcessManager{owner: owner, scope: scope, procs: map[int]*managedProcess{}, closeDone: make(chan struct{})}
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

// CleanPreviousLogs requires the durable session writer lease and runs before
// tools start. It never scans other sessions or age-based temporary paths.
func (m *ProcessManager) CleanPreviousLogs() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.logDir == "" || m.starting != 0 || len(m.procs) != 0 {
		return nil
	}
	entries, err := os.ReadDir(m.logDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "process-") && strings.HasSuffix(entry.Name(), ".log") {
			if err := os.Remove(filepath.Join(m.logDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
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

func (m *ProcessManager) startManaged(ctx context.Context, command, dir string, policy *sandbox.Sandbox, tty, pipe bool, timeout time.Duration) (int, *managedProcess, error) {
	if err := m.CheckOpen(); err != nil {
		return 0, nil, err
	}
	m.mu.Lock()
	if m.closed || m.revoking {
		m.mu.Unlock()
		return 0, nil, fmt.Errorf("process owner closed or revoking")
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
		return 0, nil, fmt.Errorf("process limit reached (32 active commands)")
	}
	m.starting++
	m.starts.Add(1)
	logDir := m.logDir
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.starting--; m.mu.Unlock(); m.starts.Done() }()
	if strings.TrimSpace(command) == "" {
		return 0, nil, fmt.Errorf("empty command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cmd, err := execution.StartShell(ctx, command, dir, execution.SanitizedEnvironmentFor(execution.EnvironmentCommand, os.Environ()), policy)
	if err != nil {
		return 0, nil, err
	}
	if logDir != "" {
		if err = os.MkdirAll(logDir, 0700); err != nil {
			execution.CleanupStartedProcess(cmd)
			return 0, nil, err
		}
	}
	log, err := os.CreateTemp(logDir, "process-*.log")
	if err != nil {
		execution.CleanupStartedProcess(cmd)
		return 0, nil, err
	}
	mp := &managedProcess{cmd: cmd, done: make(chan struct{}), changed: make(chan struct{}), log: log, logPath: log.Name(), tty: tty}
	cmd.SetIO(nil, processSink{mp}, processSink{mp})
	if tty {
		err = cmd.SetTerminal(processSink{mp})
	} else if pipe {
		_, err = cmd.StdinPipe()
	}
	cmd.SetTimeout(timeout)
	if err == nil {
		policy.MarkExternalExecution()
		err = cmd.Start()
	}
	if err != nil {
		_ = log.Close()
		_ = os.Remove(log.Name())
		execution.CleanupStartedProcess(cmd)
		return 0, nil, err
	}
	go func() {
		waitErr := cmd.Wait()
		mp.mu.Lock()
		mp.err, mp.exited, mp.finishedAt, mp.reason = waitErr, true, time.Now(), cmd.Reason()
		_ = mp.log.Close()
		mp.mu.Unlock()
		close(mp.done)
		m.mu.Lock()
		m.pruneExitedLocked()
		m.mu.Unlock()
	}()
	m.mu.Lock()
	m.next++
	id := int((uint64(m.scope) << 32) | uint64(m.next))
	m.procs[id] = mp
	if m.closed || m.revoking {
		m.mu.Unlock()
		if err := cmd.Stop("cancelled"); err != nil {
			return 0, nil, err
		}
		<-mp.done
		m.Remove(id)
		return 0, nil, fmt.Errorf("process owner closed or revoking")
	}
	m.pruneExitedLocked()
	m.mu.Unlock()
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
		id int
		mp *managedProcess
	}
	procs := make([]owned, 0, len(m.procs))
	for id, mp := range m.procs {
		procs = append(procs, owned{id: id, mp: mp})
	}
	m.mu.Unlock()

	var stopErr error
	failed := make(map[int]*managedProcess)
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
func (m *ProcessManager) pruneExitedLocked() {
	type completed struct {
		id int
		at time.Time
	}
	var exited []completed
	for id, mp := range m.procs {
		mp.mu.Lock()
		done := mp.exited
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
		m.procs[proc.id].removeLog()
		delete(m.procs, proc.id)
	}
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
	if mp.changed != nil {
		close(mp.changed)
		mp.changed = make(chan struct{})
	}
	mp.buf = append(mp.buf, fragment...)
	if len(mp.buf) > maxProcOutput {
		oldLen := len(mp.buf)
		mp.buf = append([]byte(nil), mp.buf[oldLen-maxProcOutput:]...)
	}
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
	return "", true, nil
}

func (mp *managedProcess) removeLog() {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if mp.exited && mp.logPath != "" {
		_ = os.Remove(mp.logPath)
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
			mp.removeLog()
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
	m.closeErr = closeErr
	close(m.closeDone)
	m.mu.Unlock()
	return closeErr
}

func (m *ProcessManager) belongs(pid int) bool {
	if pid <= 0 {
		return false
	}
	return uint64(pid)>>32 == uint64(m.scope)
}

// Get returns a live process by a handle created by this manager.
func (m *ProcessManager) Get(pid int) (*managedProcess, error) {
	if m == nil {
		return nil, fmt.Errorf("process: nil manager")
	}
	if !m.belongs(pid) {
		return nil, fmt.Errorf("process handle %d belongs to another session", pid)
	}
	m.mu.Lock()
	mp, ok := m.procs[pid]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no process %d (has it been stopped?)", pid)
	}
	return mp, nil
}

// Remove forgets a stopped process handle.
func (m *ProcessManager) Remove(pid int) {
	if m == nil || !m.belongs(pid) {
		return
	}
	m.mu.Lock()
	if mp := m.procs[pid]; mp != nil {
		mp.mu.Lock()
		exited := mp.exited
		mp.mu.Unlock()
		if exited {
			mp.removeLog()
			delete(m.procs, pid)
		}
	}
	m.mu.Unlock()
}
