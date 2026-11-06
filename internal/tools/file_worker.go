package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"ccdp/internal/execution"
	"ccdp/internal/fsops"
	"ccdp/internal/sandbox"
)

// Large edits are streamed by the executor in 64 KiB chunks. The file protocol
// also caps complete messages, including the 32 MiB before-image of an Edit.
const fileFrameLimit = 64 << 20

var errFileFrameTooLarge = errors.New("file worker message exceeds 64 MiB; operation was not sent")

type fileMessage struct {
	Op, Tool, Owner, WorkingDir, ReadRoot, Path, Error string
	Args                                               map[string]any
	Policy                                             *sandbox.Sandbox
	ReadLimit, OutputLimit                             int
	Versions                                           map[string]fsops.Version
	Before                                             []byte
	RecordMutation                                     bool
	Result                                             Result
}

func writeFileFrame(w io.Writer, m fileMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > fileFrameLimit {
		return errFileFrameTooLarge
	}
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(b)))
	if _, err = w.Write(head[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
func readFileFrame(r io.Reader) (fileMessage, error) {
	var m fileMessage
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n == 0 || n > fileFrameLimit {
		return m, fmt.Errorf("invalid file worker frame size %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return m, err
	}
	err := json.Unmarshal(b, &m)
	return m, err
}
func init() {
	if len(os.Args) == 2 && os.Args[1] == execution.FileWorkerArgument {
		err := serveFileWorker(os.Stdin, os.Stdout)
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}
func serveFileWorker(input io.Reader, output io.Writer) error {
	for {
		m, err := readFileFrame(input)
		if err != nil {
			return err
		}
		if m.Op != "invoke" || m.Policy == nil || m.Owner == "" {
			return fmt.Errorf("invalid file worker invocation")
		}
		// No Resources constructor: the worker owns no host temp directories,
		// process manager, session files or persistent authority.
		state := NewFileState(m.Owner)
		for path, version := range m.Versions {
			state.MarkVersion(path, version)
		}
		resources := &Resources{owner: m.Owner, Files: state}
		ctx := &Context{Context: context.Background(), Resources: resources, Owner: m.Owner,
			WorkingDir: m.WorkingDir, ReadRoot: m.ReadRoot, Sandbox: m.Policy,
			ReadLimit: m.ReadLimit, OutputLimit: m.OutputLimit}
		if m.RecordMutation {
			ctx.BeforeWrite = func(path string, before []byte) (func() error, error) {
				if err := writeFileFrame(output, fileMessage{Op: "before", Path: path, Before: before}); err != nil {
					return nil, err
				}
				ack, err := readFileFrame(input)
				if err != nil {
					return nil, err
				}
				if ack.Op != "ack" || ack.Error != "" {
					return nil, fmt.Errorf("mutation record rejected: %s", ack.Error)
				}
				return func() error {
					if err := writeFileFrame(output, fileMessage{Op: "after", Path: path}); err != nil {
						return err
					}
					ack, err := readFileFrame(input)
					if err != nil {
						return err
					}
					if ack.Op != "ack" || ack.Error != "" {
						return fmt.Errorf("mutation finalization failed: %s", ack.Error)
					}
					return nil
				}, nil
			}
		}
		var execute func(*Context, map[string]any) (Result, error)
		switch m.Tool {
		case "Read":
			execute = executeRead
		case "Write":
			execute = executeWrite
		case "Edit":
			execute = executeEdit
		case "Glob":
			execute = executeGlob
		case "LS":
			execute = executeLS
		default:
			return fmt.Errorf("unknown file operation")
		}
		result, runErr := execute(ctx, m.Args)
		reply := fileMessage{Op: "result", Result: result, Versions: map[string]fsops.Version{}}
		// Only observations produced by this call are merged back. Other concurrent
		// reads must never be replaced by an older snapshot of the complete state.
		for path, v := range state.paths {
			if old, ok := m.Versions[path]; !ok || old != v {
				reply.Versions[path] = v
			}
		}
		if runErr != nil {
			reply.Error = runErr.Error()
		}
		if err := writeFileFrame(output, reply); err != nil {
			return err
		}
	}
}

type fileWorker struct {
	profile    string
	process    *execution.Process
	input      io.WriteCloser
	output     io.ReadCloser
	diagnostic *workerDiagnostic
}

type workerDiagnostic struct {
	mu   sync.Mutex
	data []byte
}

func (d *workerDiagnostic) Write(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data = append(d.data, b[:min(len(b), 4096-len(d.data))]...)
	return len(b), nil
}
func (d *workerDiagnostic) String() string { d.mu.Lock(); defer d.mu.Unlock(); return string(d.data) }

func (w *fileWorker) close() {
	if w == nil {
		return
	}
	_ = w.input.Close()
	_ = w.output.Close()
	_ = w.process.Stop("owner_closed")
}
func (r *Resources) initFileWorkers() {
	r.fileWorkersOnce.Do(func() {
		r.fileWorkers = make(chan *fileWorker, 4)
		for i := 0; i < cap(r.fileWorkers); i++ {
			r.fileWorkers <- nil
		}
	})
}
func (r *Resources) closeFileWorkers() {
	r.initFileWorkers()
	for {
		select {
		case w := <-r.fileWorkers:
			w.close()
		default:
			return
		}
	}
}

// RetireFileWorkers is called after invocation workers have joined during a
// policy change. Replacement workers must use the newly admitted profile.
func (r *Resources) RetireFileWorkers() {
	r.initFileWorkers()
	for i := 0; i < cap(r.fileWorkers); i++ {
		select {
		case w := <-r.fileWorkers:
			w.close()
			r.fileWorkers <- nil
		default:
			return
		}
	}
}
func executeFileWorker(ctx *Context, name string, args map[string]any) (result Result, runErr error) {
	if err := ctx.checkResources(); err != nil {
		return result, err
	}
	if ctx.Resources == nil {
		return result, fmt.Errorf("file worker requires session resources")
	}
	r := ctx.Resources
	parent := ctx.Context
	if parent == nil {
		parent = context.Background()
	}
	policy := ctx.Sandbox.Snapshot()
	profile, err := execution.FileWorkerProfile(policy, ctx.ReadRoot)
	if err != nil {
		return result, err
	}
	// Coordination stays in the host. No worker is granted access to the shared
	// host lock directory or to a session's journal/checkpoint store.
	var mutationPath string
	if name == "Write" || name == "Edit" {
		mutationPath, err = ctx.ResolveWrite(StringArg(args, "file_path", ""))
		if err != nil {
			return result, err
		}
		lock, err := fsops.LockPath(mutationPath)
		if err != nil {
			return result, err
		}
		defer lock.Close()
	}
	r.initFileWorkers()
	var w *fileWorker
	select {
	case w = <-r.fileWorkers:
	case <-parent.Done():
		return result, parent.Err()
	}
	reusable := false
	defer func() {
		if !reusable || r.Closed() {
			w.close()
			w = nil
		}
		if !r.Closed() {
			r.fileWorkers <- w
		}
	}()
	if w != nil && w.profile != profile {
		w.close()
		w = nil
	}
	if w == nil {
		p, err := execution.StartFileWorker(r.OwnerContext(), policy, profile)
		if err != nil {
			return result, err
		}
		in, err := p.StdinPipe()
		if err != nil {
			execution.CleanupStartedProcess(p)
			return result, err
		}
		out, err := p.StdoutPipe()
		if err != nil {
			execution.CleanupStartedProcess(p)
			return result, err
		}
		diagnostic := &workerDiagnostic{}
		p.SetStderr(diagnostic)
		if err := p.Start(); err != nil {
			in.Close()
			out.Close()
			return result, err
		}
		w = &fileWorker{profile: profile, process: p, input: in, output: out, diagnostic: diagnostic}
	}
	state, err := ctx.fileStateForUse()
	if err != nil {
		return result, err
	}
	state.mu.Lock()
	versions := make(map[string]fsops.Version, 1)
	// Only the mutation target needs prior read authority. Read returns its
	// own observation; directory tools do not consume file versions. A long
	// session must not resend its entire file history on every operation.
	if v, exists := state.paths[mutationPath]; mutationPath != "" && exists {
		versions[mutationPath] = v
	}
	state.mu.Unlock()
	msg := fileMessage{Op: "invoke", Tool: name, Owner: ctx.Owner, WorkingDir: ctx.WorkingDir, ReadRoot: ctx.ReadRoot,
		Policy: policy, Args: args, Versions: versions, ReadLimit: ctx.ReadLimit, OutputLimit: ctx.OutputLimit,
		RecordMutation: ctx.BeforeWrite != nil && mutationPath != ""}
	if msg.Owner == "" {
		msg.Owner = r.Owner()
	}
	// Closing the local protocol pipe unblocks both directions even if the
	// executor has failed; it never causes the invocation to be replayed.
	stopped := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-parent.Done():
			_ = w.input.Close()
			_ = w.output.Close()
		case <-stopped:
		}
	}()
	defer func() { close(stopped); <-watcherDone }()
	if err := writeFileFrame(w.input, msg); err != nil {
		if errors.Is(err, errFileFrameTooLarge) {
			return result, err
		}
		return result, &execution.OutcomeUnknownError{Cause: err}
	}
	var finish func() error
	beforeSeen, published := false, false
	for {
		response, err := readFileFrame(w.output)
		if err != nil {
			return result, &execution.OutcomeUnknownError{Cause: fmt.Errorf("file worker: %w (%s)", err, w.diagnostic.String())}
		}
		switch response.Op {
		case "before", "after":
			var callbackErr error
			if mutationPath == "" || response.Path != mutationPath || ctx.BeforeWrite == nil {
				callbackErr = fmt.Errorf("file worker requested an unauthorized mutation record")
			} else if response.Op == "before" {
				if beforeSeen {
					callbackErr = fmt.Errorf("duplicate before-write record")
				} else {
					beforeSeen = true
					finish, callbackErr = ctx.BeforeWrite(response.Path, response.Before)
				}
			} else if finish == nil {
				callbackErr = fmt.Errorf("missing before-write record")
			} else {
				published = true
				callbackErr = finish()
				finish = nil
			}
			// A worker cannot publish until it receives the before-write ack.
			// Cancellation here is a known non-publication, not an unknown
			// outcome. Never send an allowing ack after the owner cancelled.
			if response.Op == "before" {
				if err := parent.Err(); err != nil {
					return result, err
				}
			}
			ack := fileMessage{Op: "ack"}
			if callbackErr != nil {
				ack.Error = callbackErr.Error()
			}
			if err := writeFileFrame(w.input, ack); err != nil {
				return result, &execution.OutcomeUnknownError{Cause: err}
			}
		case "result":
			for path, v := range response.Versions {
				state.MarkVersion(path, v)
			}
			// Once grants and changed policies are never reused for a narrower call:
			// the complete kernel profile is compared before every checkout.
			reusable = parent.Err() == nil && !policy.SingleUse
			if response.Error != "" {
				if published {
					return response.Result, &execution.OutcomeUnknownError{Cause: fmt.Errorf("file was published but completion was not confirmed: %s", response.Error)}
				}
				return response.Result, errors.New(response.Error)
			}
			return response.Result, nil
		default:
			return result, &execution.OutcomeUnknownError{Cause: fmt.Errorf("invalid file worker response")}
		}
	}
}
