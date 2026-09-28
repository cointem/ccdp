package execution

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Process owns group signalling and reaping. Keep the leader unreaped until
// group cleanup has finished, so its PID cannot be reused as an unrelated PGID.
// Callers configure streams before Start. Start owns the sole reaper; Wait
// observes the same completion as Stop and may be called more than once.
type Process struct {
	cmd         *exec.Cmd
	mu          sync.Mutex
	inputMu     sync.Mutex
	input       io.WriteCloser
	stdinRead   *os.File
	terminal    *os.File
	slave       *os.File
	output      io.Writer
	stdoutRead  *os.File
	stdoutWrite *os.File
	done        chan struct{}
	started     bool
	err         error
	reason      string
	timeout     time.Duration
	reaping     bool
	cleanup     func()
	cleanupOnce sync.Once
}

func newProcess(cmd *exec.Cmd) *Process {
	p := &Process{cmd: cmd, done: make(chan struct{})}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = PipeWaitDelay
	cmd.Cancel = func() error { return p.signal(syscall.SIGKILL, "cancelled") }
	return p
}

// SetIO configures batch input and output sinks before Start. Nil input means EOF.
func (p *Process) SetIO(input io.Reader, stdout, stderr io.Writer) {
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = input, stdout, stderr
}

func (p *Process) SetStderr(w io.Writer) { p.cmd.Stderr = w }

// StdinPipe explicitly opts into interactive/protocol input before Start.
func (p *Process) StdinPipe() (io.WriteCloser, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p.stdinRead, p.input, p.cmd.Stdin = r, w, r
	return w, nil
}

// The caller drains and closes the returned reader. Unlike exec.Cmd.StdoutPipe,
// reaping cannot close it while a protocol decoder still has buffered output.
func (p *Process) StdoutPipe() (io.ReadCloser, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p.stdoutRead, p.stdoutWrite, p.cmd.Stdout = r, w, w
	return r, nil
}

func (p *Process) SetTerminal(output io.Writer) error {
	m, s, err := openProcessPTY()
	if err != nil {
		return err
	}
	p.terminal, p.slave, p.input, p.output = m, s, m, output
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = s, s, s
	return nil
}

func (p *Process) SetTimeout(d time.Duration) { p.timeout = d }
func (p *Process) Done() <-chan struct{}      { return p.done }
func (p *Process) Reason() string             { p.mu.Lock(); defer p.mu.Unlock(); return p.reason }

func (p *Process) WriteInput(data []byte) error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	if p.input == nil {
		return fmt.Errorf("process stdin is closed; start Bash with stdin=pipe or tty=true")
	}
	if f, ok := p.input.(*os.File); ok {
		_ = f.SetWriteDeadline(time.Now().Add(5 * time.Second))
		defer f.SetWriteDeadline(time.Time{})
	}
	_, err := p.input.Write(data)
	return err
}

func (p *Process) CloseInput() error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	if p.input == nil {
		return nil
	}
	if p.terminal != nil {
		_, err := p.input.Write([]byte{4})
		return err
	}
	err := p.input.Close()
	p.input = nil
	return err
}

func (p *Process) Start() error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return fmt.Errorf("process already started")
	}
	err := p.cmd.Start()
	if p.stdinRead != nil {
		_ = p.stdinRead.Close()
	}
	if p.stdoutWrite != nil {
		_ = p.stdoutWrite.Close()
	}
	if p.slave != nil {
		_ = p.slave.Close()
	}
	if err != nil {
		p.mu.Unlock()
		if p.terminal != nil {
			_ = p.terminal.Close()
		}
		CleanupStartedProcess(p)
		return err
	}
	p.started = true
	p.mu.Unlock()
	var drained chan struct{}
	if p.terminal != nil {
		drained = make(chan struct{})
		go func() { _, _ = io.Copy(p.output, p.terminal); close(drained) }()
	}
	go func() {
		err := p.reap()
		if drained != nil {
			timer := time.AfterFunc(PipeWaitDelay, func() { _ = p.terminal.Close() })
			<-drained
			timer.Stop()
			_ = p.terminal.Close()
		}
		_ = p.CloseInput()
		CleanupStartedProcess(p)
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	}()
	if p.timeout > 0 {
		go func() {
			timer := time.NewTimer(p.timeout)
			defer timer.Stop()
			select {
			case <-p.done:
			case <-timer.C:
				_ = p.Stop("timed_out")
			}
		}()
	}
	return nil
}

func (p *Process) signal(signal syscall.Signal, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaping || p.cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-p.cmd.Process.Pid, signal)
	if err == nil && p.reason == "" {
		p.reason = reason
	}
	if err == syscall.ESRCH {
		return os.ErrProcessDone
	}
	return err
}

func (p *Process) Wait() error {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return fmt.Errorf("process not started")
	}
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Stop reports reclamation errors, not the expected signal exit status.
func (p *Process) Stop(reason string) error {
	_ = p.signal(syscall.SIGINT, reason)
	select {
	case <-p.done:
		return nil
	case <-time.After(2 * time.Second):
	}
	err := p.signal(syscall.SIGKILL, reason)
	select {
	case <-p.done:
		return nil
	case <-time.After(4 * time.Second):
		return fmt.Errorf("process did not finish reclamation after stop (signal: %v)", err)
	}
}

func (p *Process) reap() error {
	// Observe exit without consuming the wait status. Unlike Wait followed by
	// kill(-pid), the leader still reserves its PID throughout group cleanup.
	observeErr := waitProcessExit(p.cmd.Process.Pid)
	p.mu.Lock()
	if !p.reaping {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		p.reaping = true
	}
	p.mu.Unlock()
	err := p.cmd.Wait()
	if err != nil {
		return err
	}
	return observeErr
}

func (p *Process) Run() error {
	if err := p.Start(); err != nil {
		CleanupStartedProcess(p)
		return err
	}
	return p.Wait()
}
