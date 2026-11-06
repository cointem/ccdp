package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/runtimepath"
)

const workerArgument = "--ccdp-internal-computer"

func init() {
	if len(os.Args) != 2 || os.Args[1] != workerArgument {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var req Request
	dec := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		os.Exit(2)
	}
	resp := runWorker(ctx, req, nativeCall)
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// RequestAccess is called only in response to the launcher's explicit flag.
// Model invocations preflight access without triggering OS prompts.
func RequestAccess(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := callWorker(ctx, Request{Operation: "authorize"}, nil)
	if err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	if r.Status == nil {
		return errors.New("Computer permission probe returned no status")
	}
	return r.Status.Check()
}

func callWorker(ctx context.Context, req Request, lease *os.File) (Response, error) {
	var resp Response
	exe, err := os.Executable()
	if err != nil {
		return resp, err
	}
	wire, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	cmd := exec.CommandContext(ctx, exe, workerArgument)
	// No provider credentials, shell startup files, project environment or cwd.
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8"}
	cmd.Stdin = bytes.NewReader(wire)
	// Retain the desktop lock if the parent crashes while input is in flight.
	// The helper owns the inherited descriptor until its bounded request exits.
	if lease != nil {
		cmd.ExtraFiles = []*os.File{lease}
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	// A native input primitive always releases its keys/buttons before the
	// worker observes cancellation. Allow it to finish before forced cleanup.
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return resp, err
	}
	if err := cmd.Start(); err != nil {
		return resp, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, 4<<20))
	_ = stdout.Close()
	waitErr := cmd.Wait()
	decodeErr := json.Unmarshal(data, &resp)
	// A complete response is authoritative even if cancellation raced exit.
	if readErr == nil && decodeErr == nil {
		return resp, nil
	}
	err = errors.Join(readErr, waitErr, decodeErr)
	if req.Operation == "act" {
		return resp, &execution.OutcomeUnknownError{Cause: err}
	}
	return resp, fmt.Errorf("Computer worker failed: %w", err)
}

func runWorker(ctx context.Context, req Request, native func(map[string]any) Response) Response {
	if req.Operation == "status" || req.Operation == "authorize" {
		return native(map[string]any{"operation": req.Operation})
	}
	if req.Operation != "screenshot" && req.Operation != "act" {
		return Response{Error: "unsupported Computer operation"}
	}
	resp := native(map[string]any{"operation": "status"})
	if resp.Error != "" {
		return resp
	}
	if resp.Status == nil {
		return Response{Error: "missing desktop status"}
	}
	if err := resp.Status.Check(); err != nil {
		resp.Error = err.Error()
		return resp
	}
	completed := 0
	if req.Operation == "act" {
		if req.Frame == nil || req.Frame.Display != resp.Status.Display {
			return Response{Error: "display changed; take a fresh screenshot before acting"}
		}
		if err := validateActions(req.Actions, *req.Frame); err != nil {
			return Response{Error: err.Error()}
		}
		for _, action := range req.Actions {
			if err := ctx.Err(); err != nil {
				return Response{Completed: completed, Error: "cancelled; inspect a fresh screenshot before continuing; do not repeat completed actions"}
			}
			if action.Type == "wait" {
				select {
				case <-time.After(time.Duration(action.Milliseconds) * time.Millisecond):
				case <-ctx.Done():
					return Response{Completed: completed, Error: "cancelled during wait; do not repeat completed actions"}
				}
			} else {
				// Convert screenshot pixels to global display points (including
				// Retina scaling and nonzero display origins) only in the worker.
				point := func(p []float64) []float64 {
					if len(p) == 0 {
						return nil
					}
					return []float64{req.Frame.Display.X + p[0]*req.Frame.Display.Width/float64(req.Frame.Width), req.Frame.Display.Y + p[1]*req.Frame.Display.Height/float64(req.Frame.Height)}
				}
				action.Position, action.To = point(action.Position), point(action.To)
				key, flags, _ := parseKey(action.Key)
				r := native(map[string]any{"operation": "action", "action": action, "key_code": key, "key_flags": flags})
				if r.Error != "" {
					r.Completed = completed
					return r
				}
			}
			completed++
		}
		// Give the application one short paint interval, not a model polling loop.
		select {
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		return Response{Completed: completed, Error: "cancelled after actions; take a fresh screenshot, do not repeat completed actions"}
	}
	resp = native(map[string]any{"operation": "screenshot"})
	resp.Completed = completed
	if completed > 0 && resp.Error != "" {
		resp.Error = "Actions completed; screenshot failed: " + resp.Error + ". Take a fresh screenshot; do not repeat actions."
	}
	return resp
}

// The lease spans observations and actions so two sessions cannot race the
// same desktop. OS locks are released even when the owning ccdp process dies.
func lockDesktop() (*os.File, error) {
	dir, err := runtimepath.EnsurePrivate(runtimepath.DefaultRoot())
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(filepath.Join(dir, "computer.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "computer.lock")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, errors.New("Computer lock must be a private regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another ccdp session owns the desktop; close that session before using Computer")
	}
	return file, nil
}
