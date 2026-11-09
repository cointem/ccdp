package execution

// The controller is a private, trusted child of the session host. Only the
// controller launches sandbox-exec; project processes never inherit its IPC.
import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"ccdp/internal/runtimepath"
	"ccdp/internal/sandbox"
)

const executorArgument = "--ccdp-internal-executor-v2"
const wireLimit = 1 << 20
const streamChunk = 64 << 10

var ErrEnvironmentUnavailable = errors.New("execution environment unavailable")

// OutcomeUnknownError is terminal and must never trigger an automatic replay.
type OutcomeUnknownError struct{ Cause error }

func (e *OutcomeUnknownError) Error() string {
	return "execution outcome unknown; do not replay: " + e.Cause.Error()
}
func (e *OutcomeUnknownError) Unwrap() error { return e.Cause }

type wireMessage struct {
	Version       int
	Seq, ID       uint64
	Op            string
	Args, Env     []string
	Roots         []string
	Dir           string
	TTY, Input    bool
	Timeout       time.Duration
	Data          []byte
	Count         uint64
	Exit          int
	Error, Reason string
}

func encodeFrame(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > wireLimit {
		return nil, fmt.Errorf("executor frame exceeds %d bytes", wireLimit)
	}
	frame := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(b)))
	copy(frame[4:], b)
	return frame, nil
}
func writeFrame(w io.Writer, v any) error {
	frame, err := encodeFrame(v)
	if err != nil {
		return err
	}
	_, err = w.Write(frame)
	return err
}
func readFrame(r io.Reader, v any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > wireLimit {
		return fmt.Errorf("invalid executor frame size %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// The internal entry is also available in Go test binaries. It runs before
// application initialization, so no provider keys or plugins are loaded here.
func init() {
	if len(os.Args) == 2 && os.Args[1] == executorArgument {
		out := os.NewFile(3, "executor-output")
		if out == nil {
			os.Exit(2)
		}
		syscall.CloseOnExec(int(out.Fd()))
		err := serveExecutor(os.Stdin, os.Stdout, out)
		if err != nil && !errors.Is(err, io.EOF) {
			os.Exit(1)
		}
		os.Exit(0)
	}
}

var controllers sync.Map

// Controller is shared by a root session tree. Close is performed by its root;
// child owners hold only its ID, never authority to close a sibling's process.
type Controller struct {
	ID               string
	privateRoot      string
	mu               sync.Mutex
	sendGate         chan struct{}
	done             chan struct{}
	ready            chan struct{}
	readyOnce        sync.Once
	isReady          bool
	seq              uint64
	root, binary     string
	environments     []string
	cmd              *exec.Cmd
	input            io.WriteCloser
	output, stream   io.ReadCloser
	pending          map[uint64]chan wireMessage
	processes        map[uint64]*remoteProcess
	blockedRoots     []string
	quarantineScopes map[string][]string
	closed           bool
	failure          error
	closeDone        chan struct{}
	closeErr         error
}

func NewController(roots ...string) *Controller {
	id, err := runtimepath.ID()
	c := &Controller{ID: id,
		pending: map[uint64]chan wireMessage{}, processes: map[uint64]*remoteProcess{}, sendGate: make(chan struct{}, 1), done: make(chan struct{}), ready: make(chan struct{})}
	root := runtimepath.DefaultRoot()
	if len(roots) > 0 && roots[0] != "" {
		root = roots[0]
	}
	if err == nil {
		c.privateRoot, err = runtimepath.EnsurePrivate(root)
	}
	if err == nil {
		c.root, err = runtimepath.EnsurePrivate(filepath.Join(c.privateRoot, "executors", id))
	}
	if err != nil {
		c.failure = fmt.Errorf("%w: private runtime preparation: %v", ErrEnvironmentUnavailable, err)
	}
	controllers.Store(c.ID, c)
	return c
}

func NewEnvironment(executorID string) (string, error) {
	c := controllerFor(executorID)
	if c == nil {
		return "", ErrEnvironmentUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return "", c.failure
	}
	if c.closed {
		return "", ErrEnvironmentUnavailable
	}
	id, err := runtimepath.ID()
	if err != nil {
		return "", err
	}
	parent, err := runtimepath.EnsurePrivate(runtimepath.TemporaryRoot())
	if err != nil {
		return "", err
	}
	root, err := runtimepath.EnsurePrivate(filepath.Join(parent, id))
	if err != nil {
		return "", err
	}
	for _, part := range []string{"scratch", "logs"} {
		if _, err := runtimepath.EnsurePrivate(filepath.Join(root, part)); err != nil {
			_ = os.RemoveAll(root)
			return "", err
		}
	}
	c.environments = append(c.environments, root)
	return root, nil
}

func BindPrivateRoots(policy *sandbox.Sandbox) {
	if policy == nil {
		return
	}
	policy.AddPrivateRoot(runtimepath.DefaultRoot())
	policy.AddPrivateRoot(runtimepath.TemporaryRoot())
	if c := controllerFor(policy.ExecutorID); c != nil {
		c.mu.Lock()
		root := c.privateRoot
		c.mu.Unlock()
		policy.AddPrivateRoot(root)
	}
}
func controllerFor(id string) *Controller {
	if id == "" {
		return nil
	}
	v, ok := controllers.Load(id)
	if !ok {
		return nil
	}
	return v.(*Controller)
}

func (c *Controller) ensure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("executor closed")
	}
	if c.failure != nil {
		return c.failure
	}
	if c.cmd != nil {
		return nil
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	root, err := runtimepath.EnsurePrivate(filepath.Join(c.root, "control"))
	if err != nil {
		return err
	}
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		root = canonical
	}
	pinned := filepath.Join(root, "ccdp-worker")
	in, err := os.Open(source)
	if err != nil {
		_ = os.RemoveAll(root)
		return err
	}
	out, err := os.OpenFile(pinned, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0500)
	if err == nil {
		_, err = io.Copy(out, in)
		err = errors.Join(err, out.Close())
	}
	_ = in.Close()
	if err != nil {
		_ = os.RemoveAll(root)
		return err
	}
	// Start the fixed binary with a minimal environment. Per-command sanitized
	// environments arrive over the private channel, not through this process.
	cmd := exec.Command(pinned, executorArgument)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C"}
	cmd.Dir = "/"
	streamR, streamW, err := os.Pipe()
	if err != nil {
		_ = os.RemoveAll(root)
		return err
	}
	cmd.ExtraFiles = []*os.File{streamW}
	input, err := cmd.StdinPipe()
	if err != nil {
		streamR.Close()
		streamW.Close()
		os.RemoveAll(root)
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		streamR.Close()
		streamW.Close()
		os.RemoveAll(root)
		return err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		streamR.Close()
		streamW.Close()
		os.RemoveAll(root)
		return err
	}
	_ = streamW.Close()
	c.binary, c.cmd, c.input, c.output, c.stream = pinned, cmd, input, output, streamR
	go c.readControl()
	go c.readOutput()
	go func() {
		err := cmd.Wait()
		if err == nil {
			err = io.EOF
		}
		c.fail(fmt.Errorf("executor disconnected; execution outcome may be unknown: %w", err))
		close(c.done)
	}()
	return nil
}

func (c *Controller) Binary() (string, error) {
	if err := c.ensure(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.binary, nil
}
func (c *Controller) Protect(policy *sandbox.Sandbox) error {
	if err := c.ensure(); err != nil {
		return err
	}
	c.mu.Lock()
	root, binary := c.root, c.binary
	c.mu.Unlock()
	BindPrivateRoots(policy)
	policy.AddProtectedDir(filepath.Join(root, "control"))
	policy.AddProtectedDir(filepath.Join(root, "config"))
	policy.AddRuntimeReadFile(binary)
	return nil
}
func ExecutorBinary(policy *sandbox.Sandbox) (string, error) {
	if policy == nil || controllerFor(policy.ExecutorID) == nil {
		return "", fmt.Errorf("execution environment unavailable")
	}
	return controllerFor(policy.ExecutorID).Binary()
}

func (c *Controller) request(ctx context.Context, msg wireMessage) (wireMessage, error) {
	return c.requestProcess(ctx, msg, nil)
}

func (c *Controller) requestProcess(ctx context.Context, msg wireMessage, process *remoteProcess) (wireMessage, error) {
	if err := ctx.Err(); err != nil {
		return wireMessage{}, err
	}
	if err := c.ensure(); err != nil {
		return wireMessage{}, err
	}
	// Cold-start/signature verification belongs to preparation. No request
	// bytes or process identity are dispatched before the v1 handshake.
	readyTimer := time.NewTimer(30 * time.Second)
	defer readyTimer.Stop()
	select {
	case <-c.ready:
	case <-ctx.Done():
		return wireMessage{}, ctx.Err()
	case <-readyTimer.C:
		err := fmt.Errorf("%w: executor did not become ready before preparation deadline", ErrEnvironmentUnavailable)
		c.fail(err)
		return wireMessage{}, err
	}
	select {
	case c.sendGate <- struct{}{}:
	case <-ctx.Done():
		return wireMessage{}, ctx.Err()
	}
	c.mu.Lock()
	if c.failure != nil || c.closed {
		err := c.failure
		if err == nil {
			err = fmt.Errorf("executor closed")
		}
		c.mu.Unlock()
		<-c.sendGate
		return wireMessage{}, err
	}
	if len(c.pending) >= 256 {
		c.mu.Unlock()
		<-c.sendGate
		return wireMessage{}, fmt.Errorf("executor request capacity exhausted")
	}
	if process != nil && rootsOverlap(process.roots, c.blockedRoots) {
		c.mu.Unlock()
		<-c.sendGate
		return wireMessage{}, fmt.Errorf("execution environment quarantined; process was not started")
	}
	c.seq++
	msg.Seq = c.seq
	if process != nil {
		// Process identity is the submission sequence within this generation.
		// A completed ID cannot be submitted again with a new sequence.
		msg.ID = msg.Seq
	}
	frame, err := encodeFrame(msg)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		c.mu.Unlock()
		<-c.sendGate
		return wireMessage{}, err
	}
	if process != nil {
		process.id = msg.ID
		c.processes[msg.ID] = process
	}
	reply := make(chan wireMessage, 1)
	c.pending[msg.Seq] = reply
	input := c.input
	c.mu.Unlock()
	if f, ok := input.(*os.File); ok {
		deadline := time.Now().Add(5 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = f.SetWriteDeadline(deadline)
	}
	_, err = input.Write(frame)
	<-c.sendGate
	if err != nil {
		c.fail(err)
	}
	defer func() { c.mu.Lock(); delete(c.pending, msg.Seq); c.mu.Unlock() }()
	select {
	case r := <-reply:
		if r.Error != "" {
			if r.Op == "lost" {
				return r, &OutcomeUnknownError{Cause: errors.New(r.Error)}
			}
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-ctx.Done():
		return wireMessage{}, &OutcomeUnknownError{Cause: ctx.Err()}
	}
}
func (c *Controller) readControl() {
	for {
		var m wireMessage
		if err := readFrame(c.output, &m); err != nil {
			c.fail(err)
			return
		}
		if m.Op == "ready" {
			if m.Version != 2 {
				c.fail(errors.New("incompatible executor protocol"))
				return
			}
			c.mu.Lock()
			c.isReady = true
			c.mu.Unlock()
			c.readyOnce.Do(func() { close(c.ready) })
			continue
		}
		c.mu.Lock()
		ch, p := c.pending[m.Seq], c.processes[m.ID]
		c.mu.Unlock()
		if m.Op == "done" && p != nil {
			p.finished(m)
			continue
		}
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
	}
}
func (c *Controller) readOutput() {
	for {
		var m wireMessage
		if err := readFrame(c.stream, &m); err != nil {
			c.fail(err)
			return
		}
		c.mu.Lock()
		p := c.processes[m.ID]
		c.mu.Unlock()
		if p != nil {
			select {
			case p.chunks <- m:
			case <-p.process.done:
			case <-time.After(5 * time.Second):
				p.fail(fmt.Errorf("executor output consumer exceeded its bounded queue; outcome unknown"))
				go func(id uint64) {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					defer cancel()
					_, _ = c.request(ctx, wireMessage{ID: id, Op: "stop", Reason: "resource_limit"})
				}(m.ID)
			}
		}
	}
}
func (c *Controller) fail(err error) {
	c.mu.Lock()
	if c.failure != nil {
		c.mu.Unlock()
		return
	}
	c.failure = fmt.Errorf("execution environment unavailable: %w", err)
	c.readyOnce.Do(func() { close(c.ready) })
	for _, ch := range c.pending {
		select {
		case ch <- wireMessage{Op: "lost", Error: c.failure.Error()}:
		default:
		}
	}
	ps := make([]*remoteProcess, 0, len(c.processes))
	for _, p := range c.processes {
		ps = append(ps, p)
	}
	c.mu.Unlock()
	for _, p := range ps {
		if p.process.executionPolicy != nil {
			p.process.executionPolicy.MarkExternalExecution()
		}
		if p.policy != nil && p.policy.ExternalExecutionPossible() {
			if recordErr := p.policy.RecordQuarantine("executor disconnected before workload cleanup was confirmed"); recordErr != nil {
				c.mu.Lock()
				c.failure = errors.Join(c.failure, recordErr)
				c.mu.Unlock()
			}
		}
		p.fail(fmt.Errorf("execution outcome unknown: %w", err))
	}
}
func (c *Controller) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.closeErr
	}
	c.closed = true
	c.closeDone = make(chan struct{})
	in, out, stream, root, cmd := c.input, c.output, c.stream, c.root, c.cmd
	environments := append([]string(nil), c.environments...)
	priorFailure := c.failure
	c.mu.Unlock()
	controllers.Delete(c.ID)
	if in != nil {
		_ = in.Close()
	}
	closeErr := priorFailure
	if cmd != nil {
		select {
		case <-c.done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			closeErr = fmt.Errorf("executor did not confirm shutdown; descendant outcome unknown")
		}
	}
	c.fail(fmt.Errorf("executor owner closed"))
	if out != nil {
		_ = out.Close()
	}
	if stream != nil {
		_ = stream.Close()
	}
	// The pinned binary may still be in use by workers. Removing its directory
	// does not claim that detached descendants have been reclaimed.
	if root != "" && closeErr == nil {
		closeErr = errors.Join(closeErr, os.RemoveAll(root))
		for _, environment := range environments {
			closeErr = errors.Join(closeErr, os.RemoveAll(environment))
		}
	}
	c.mu.Lock()
	c.closeErr = closeErr
	close(c.closeDone)
	c.mu.Unlock()
	return closeErr
}

type processExitError struct {
	code    int
	message string
}

func (e *processExitError) Error() string { return e.message }
func (e *processExitError) ExitCode() int { return e.code }
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e interface{ ExitCode() int }
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}

type remoteProcess struct {
	roots      []string
	policy     *sandbox.Sandbox
	controller *Controller
	process    *Process
	context    context.Context
	id         uint64
	tty        bool
	chunks     chan wireMessage
	mu         sync.Mutex
	final      *wireMessage
	count      uint64
	once       sync.Once
}

func bindController(p *Process, ctx context.Context, policy *sandbox.Sandbox) *Process {
	c := controllerFor(policy.ExecutorID)
	p.remote = &remoteProcess{controller: c, process: p, context: ctx, policy: policy.Snapshot(), roots: append([]string{policy.Workspace}, policy.AdditionalDirs...), chunks: make(chan wireMessage, 64)}
	return p
}
func (r *remoteProcess) start() error {
	p := r.process
	if r.controller == nil {
		return fmt.Errorf("execution environment no longer exists")
	}
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return fmt.Errorf("process already started")
	}
	p.started = true
	p.mu.Unlock()
	c := r.controller
	if err := c.ensure(); err != nil {
		r.finish(err, "not_started")
		return err
	}
	go r.drain()
	ctx := r.context
	if ctx == nil {
		ctx = context.Background()
	}
	reply, err := c.requestProcess(ctx, wireMessage{Op: "start", Args: p.cmd.Args, Env: p.cmd.Env,
		Dir: p.cmd.Dir, Roots: r.roots, TTY: r.tty, Input: p.cmd.Stdin != nil || r.tty, Timeout: p.timeout}, r)
	if err != nil {
		var unknown *OutcomeUnknownError
		if errors.As(err, &unknown) {
			if p.executionPolicy != nil {
				p.executionPolicy.MarkExternalExecution()
			}
			r.fail(err)
			go r.stop("cancelled")
		} else {
			r.finish(err, "not_started")
		}
		return err
	}
	_ = reply
	if p.executionPolicy != nil {
		p.executionPolicy.MarkExternalExecution()
	}
	if p.cmd.Stdin != nil {
		go func() {
			buf := make([]byte, streamChunk)
			for {
				n, err := p.cmd.Stdin.Read(buf)
				if n > 0 {
					if e := r.write(buf[:n]); e != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
			_ = r.closeInput()
		}()
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = r.stop("cancelled")
		case <-p.done:
		}
	}()
	return nil
}
func (r *remoteProcess) drain() {
	for {
		select {
		case m := <-r.chunks:
			w := r.process.cmd.Stdout
			if m.Op == "stderr" {
				w = r.process.cmd.Stderr
			}
			if w != nil {
				if _, err := w.Write(m.Data); err != nil {
					r.fail(err)
					return
				}
			}
			r.mu.Lock()
			r.count++
			r.mu.Unlock()
			r.complete()
		case <-r.process.done:
			return
		}
	}
}
func (r *remoteProcess) finished(m wireMessage) {
	r.mu.Lock()
	r.final = &m
	r.mu.Unlock()
	r.complete()
}
func (r *remoteProcess) complete() {
	r.mu.Lock()
	m := r.final
	ready := m != nil && r.count >= m.Count
	r.mu.Unlock()
	if !ready {
		return
	}
	var err error
	if m.Exit != 0 {
		err = &processExitError{m.Exit, m.Error}
	}
	r.finish(err, m.Reason)
}
func (r *remoteProcess) fail(err error) { r.finish(&OutcomeUnknownError{Cause: err}, "unknown") }
func (r *remoteProcess) finish(err error, reason string) {
	r.once.Do(func() {
		p := r.process
		p.mu.Lock()
		p.err, p.reason = err, reason
		p.mu.Unlock()
		if p.stdinRead != nil {
			_ = p.stdinRead.Close()
		}
		p.inputMu.Lock()
		if p.input != nil {
			_ = p.input.Close()
			p.input = nil
		}
		p.inputMu.Unlock()
		if p.stdoutWrite != nil {
			_ = p.stdoutWrite.Close()
		}
		CleanupStartedProcess(p)
		close(p.done)
		if r.controller != nil {
			r.controller.mu.Lock()
			delete(r.controller.processes, r.id)
			r.controller.mu.Unlock()
		}
	})
}
func (r *remoteProcess) action(op string, data []byte, reason string) error {
	if r.controller == nil {
		return fmt.Errorf("execution environment unavailable")
	}
	select {
	case <-r.process.done:
		if op == "eof" {
			return nil
		}
		if op != "stop" {
			return fmt.Errorf("process has exited")
		}
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := r.controller.request(ctx, wireMessage{Op: op, ID: r.id, Data: data, Reason: reason})
	return err
}
func (r *remoteProcess) write(b []byte) error {
	for len(b) > 0 {
		n := min(len(b), streamChunk)
		if err := r.action("write", b[:n], ""); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
func (r *remoteProcess) closeInput() error        { return r.action("eof", nil, "") }
func (r *remoteProcess) stop(reason string) error { return r.action("stop", nil, reason) }

type serverProcess struct {
	roots                     []string
	p                         *Process
	mu                        sync.Mutex
	count                     uint64
	input                     chan wireMessage
	stopMu                    sync.Mutex
	stopStarted, stopComplete bool
	stopError                 string
	stopWaiters               []wireMessage
}
type executorSink struct {
	sp   *serverProcess
	id   uint64
	kind string
	send func(wireMessage) error
}

func (w executorSink) Write(b []byte) (int, error) {
	total := len(b)
	w.sp.mu.Lock()
	defer w.sp.mu.Unlock()
	for len(b) > 0 {
		n := min(len(b), streamChunk)
		if err := w.send(wireMessage{ID: w.id, Op: w.kind, Data: b[:n]}); err != nil {
			return total - len(b), err
		}
		w.sp.count++
		b = b[n:]
	}
	return total, nil
}

func serveExecutor(input io.Reader, control, output io.Writer) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var controlMu, outputMu, mu sync.Mutex
	send := func(m wireMessage) error { controlMu.Lock(); defer controlMu.Unlock(); return writeFrame(control, m) }
	stream := func(m wireMessage) error { outputMu.Lock(); defer outputMu.Unlock(); return writeFrame(output, m) }
	if err := send(wireMessage{Op: "ready", Version: 2}); err != nil {
		return err
	}
	procs := map[uint64]*serverProcess{}
	var blockedRoots []string
	quarantineScopes := map[string][]string{}
	var highwater uint64
	defer func() {
		cancel()
		mu.Lock()
		var active []*Process
		for _, sp := range procs {
			active = append(active, sp.p)
		}
		mu.Unlock()
		var wg sync.WaitGroup
		for _, p := range active {
			wg.Add(1)
			go func(p *Process) { defer wg.Done(); _ = p.Stop("owner_closed") }(p)
		}
		wg.Wait()
	}()
	for {
		var m wireMessage
		if err := readFrame(input, &m); err != nil {
			return err
		}
		if m.Seq <= highwater {
			_ = send(wireMessage{Seq: m.Seq, ID: m.ID, Error: "executor request already consumed; no replay"})
			continue
		}
		highwater = m.Seq
		if m.Op == "quarantine" || m.Op == "resume" {
			if m.Op == "quarantine" {
				quarantineScopes[m.Dir] = m.Roots
			} else {
				delete(quarantineScopes, m.Dir)
			}
			blockedRoots = nil
			for _, roots := range quarantineScopes {
				blockedRoots = append(blockedRoots, roots...)
			}
			_ = send(wireMessage{Seq: m.Seq, Op: "ack"})
			continue
		}
		if m.Op == "start" {
			mu.Lock()
			_, duplicate := procs[m.ID]
			full := len(procs) >= 128
			mu.Unlock()
			if duplicate || full || rootsOverlap(m.Roots, blockedRoots) || m.ID != m.Seq || len(m.Args) < 4 || m.Args[0] != sandbox.BackendPath || m.Args[1] != "-p" {
				_ = send(wireMessage{Seq: m.Seq, ID: m.ID, Error: "invalid, duplicate, or over-limit sandbox execution"})
				continue
			}
			p := newProcess(exec.CommandContext(ctx, sandbox.BackendPath, m.Args[1:]...))
			p.cmd.Env, p.cmd.Dir = m.Env, m.Dir
			p.SetTimeout(m.Timeout)
			sp := &serverProcess{p: p, roots: m.Roots, input: make(chan wireMessage, 8)}
			stdout := executorSink{sp, m.ID, "stdout", stream}
			stderr := executorSink{sp, m.ID, "stderr", stream}
			p.SetIO(nil, stdout, stderr)
			var err error
			if m.TTY {
				err = p.SetTerminal(stdout)
			} else if m.Input {
				_, err = p.StdinPipe()
			}
			if err == nil {
				err = p.Start()
			}
			if err != nil {
				CleanupStartedProcess(p)
				_ = send(wireMessage{Seq: m.Seq, ID: m.ID, Error: err.Error()})
				continue
			}
			mu.Lock()
			procs[m.ID] = sp
			mu.Unlock()
			_ = send(wireMessage{Seq: m.Seq, ID: m.ID, Op: "started"})
			// Serialize stdin writes and EOF in wire order. The bounded queue
			// never blocks stop/quarantine or another process's control channel.
			go func(sp *serverProcess) {
				for {
					select {
					case op := <-sp.input:
						var err error
						if op.Op == "eof" {
							err = sp.p.CloseInput()
						} else {
							err = sp.p.WriteInput(op.Data)
						}
						reply := wireMessage{Seq: op.Seq, ID: op.ID, Op: "ack"}
						if err != nil {
							reply.Error = err.Error()
						}
						_ = send(reply)
					case <-sp.p.Done():
						return
					case <-ctx.Done():
						return
					}
				}
			}(sp)
			go func(m wireMessage, sp *serverProcess) {
				err := sp.p.Wait()
				sp.mu.Lock()
				count := sp.count
				sp.mu.Unlock()
				result := wireMessage{ID: m.ID, Op: "done", Count: count, Exit: ExitCode(err), Reason: sp.p.Reason()}
				if err != nil {
					result.Error = err.Error()
				}
				_ = send(result)
				mu.Lock()
				delete(procs, m.ID)
				mu.Unlock()
			}(m, sp)
			continue
		}
		mu.Lock()
		sp := procs[m.ID]
		mu.Unlock()
		reply := wireMessage{Seq: m.Seq, ID: m.ID, Op: "ack"}
		if sp == nil {
			if m.Op != "stop" && m.Op != "eof" {
				reply.Error = "process handle unavailable"
			}
		} else if m.Op == "write" || m.Op == "eof" {
			select {
			case sp.input <- m:
				continue
			default:
				reply.Error = "process input queue full; input was not sent"
			}
		} else if m.Op == "stop" {
			sp.stopMu.Lock()
			if sp.stopComplete {
				reply.Error = sp.stopError
				sp.stopMu.Unlock()
			} else if len(sp.stopWaiters) >= 256 {
				sp.stopMu.Unlock()
				reply.Error = "process stop waiter capacity exhausted"
			} else {
				sp.stopWaiters = append(sp.stopWaiters, m)
				start := !sp.stopStarted
				sp.stopStarted = true
				sp.stopMu.Unlock()
				if start {
					go func(sp *serverProcess, reason string) {
						err := sp.p.Stop(reason)
						sp.stopMu.Lock()
						sp.stopComplete = true
						if err != nil {
							sp.stopError = err.Error()
						}
						waiters, stopError := sp.stopWaiters, sp.stopError
						sp.stopWaiters = nil
						sp.stopMu.Unlock()
						for _, waiter := range waiters {
							_ = send(wireMessage{Seq: waiter.Seq, ID: waiter.ID, Op: "ack", Error: stopError})
						}
					}(sp, m.Reason)
				}
				continue
			}
		} else {
			reply.Error = "unknown executor operation"
		}
		_ = send(reply)
	}
}

// Quarantine closes dispatch before acknowledging a policy withdrawal.
func Quarantine(ctx context.Context, policy *sandbox.Sandbox) error {
	if policy == nil || policy.ExecutorID == "" {
		return ErrEnvironmentUnavailable
	}
	c := controllerFor(policy.ExecutorID)
	if c == nil {
		return ErrEnvironmentUnavailable
	}
	roots := policy.ExecutionRoots()
	c.mu.Lock()
	if c.quarantineScopes == nil {
		c.quarantineScopes = map[string][]string{}
	}
	c.quarantineScopes[policy.Workspace] = roots
	c.blockedRoots = append(c.blockedRoots, roots...)
	started := c.cmd != nil
	c.mu.Unlock()
	if !started {
		return nil
	}
	_, err := c.request(ctx, wireMessage{Op: "quarantine", Dir: policy.Workspace, Roots: roots})
	return err
}
func rootsOverlap(left, right []string) bool {
	for _, a := range left {
		for _, b := range right {
			if a != "" && b != "" && (withinPath(a, b) || withinPath(b, a)) {
				return true
			}
		}
	}
	return false
}

// ResumeAdmission is only called after managed cleanup has completed and no
// detached-workload witness remains. A durable quarantine cannot be cleared.
func ResumeAdmission(policy *sandbox.Sandbox) error {
	if policy == nil || policy.ExecutorID == "" {
		return ErrEnvironmentUnavailable
	}
	if err := policy.CheckExecution(); err != nil {
		return err
	}
	c := controllerFor(policy.ExecutorID)
	if c == nil {
		return ErrEnvironmentUnavailable
	}
	c.mu.Lock()
	started := c.cmd != nil
	c.mu.Unlock()
	if started {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if _, err := c.request(ctx, wireMessage{Op: "resume", Dir: policy.Workspace}); err != nil {
			return err
		}
	}
	c.mu.Lock()
	delete(c.quarantineScopes, policy.Workspace)
	c.blockedRoots = nil
	for _, roots := range c.quarantineScopes {
		c.blockedRoots = append(c.blockedRoots, roots...)
	}
	c.mu.Unlock()
	return nil
}

// EnvironmentStatus is a bounded diagnostic snapshot, independent of model
// transport and chat state. It performs no environment preparation.
func EnvironmentStatus(policy *sandbox.Sandbox) (string, string) {
	if policy == nil {
		return "unavailable", "missing execution policy"
	}
	if policy.ExecutorID == "" {
		return "unavailable", "missing executor binding"
	}
	c := controllerFor(policy.ExecutorID)
	if c == nil {
		return "unavailable", "executor generation has closed"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "unavailable", "executor generation has closed"
	}
	if c.failure != nil {
		return "degraded", c.failure.Error()
	}
	if rootsOverlap(append([]string{policy.Workspace}, policy.AdditionalDirs...), c.blockedRoots) {
		return "quarantined", "project execution is paused pending cleanup"
	}
	if c.cmd == nil {
		return "unprepared", "environment will be prepared on first execution"
	}
	if !c.isReady {
		return "preparing", "executor is initializing"
	}
	return "ready", ""
}

func CheckEnvironment(policy *sandbox.Sandbox) error {
	if err := policy.CheckExecution(); err != nil {
		return fmt.Errorf("%w: %v", ErrEnvironmentUnavailable, err)
	}
	state, reason := EnvironmentStatus(policy)
	if state == "degraded" || state == "unavailable" || state == "quarantined" {
		return fmt.Errorf("%w: %s", ErrEnvironmentUnavailable, reason)
	}
	return nil
}
