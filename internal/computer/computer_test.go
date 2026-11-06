package computer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccdp/internal/execution"
)

func testFrame() Frame {
	return Frame{ID: "observation", Width: 1000, Height: 500, Display: Display{ID: 1, X: -200, Y: 100, Width: 2000, Height: 1000}, CapturedAt: time.Now()}
}

func TestControllerRequiresFreshObservationAndDoesNotReplay(t *testing.T) {
	for _, outcome := range []string{"success", "partial", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			calls, locks := 0, 0
			var lease *os.File
			c := &Controller{acquire: func() (*os.File, error) {
				locks++
				var err error
				lease, err = os.CreateTemp(t.TempDir(), "lease")
				return lease, err
			}, call: func(_ context.Context, req Request, _ *os.File) (Response, error) {
				calls++
				if req.Operation == "act" {
					if req.Frame == nil {
						t.Fatal("missing trusted frame")
					}
					if outcome == "unknown" {
						return Response{}, &execution.OutcomeUnknownError{Cause: errors.New("worker died")}
					}
					if outcome == "partial" {
						return Response{Completed: 1, Error: "interrupted"}, nil
					}
				}
				f := testFrame()
				return Response{Frame: &f, Image: []byte("jpeg")}, nil
			}}
			ctx := context.Background()
			req := Request{Operation: "act", FrameID: "invented", Actions: []Action{{Type: "key", Key: "cmd+l"}}}
			if _, err := c.Run(ctx, req); err == nil || calls != 0 || locks != 0 {
				t.Fatal("acted without screenshot")
			}
			r, err := c.Run(ctx, Request{Operation: "screenshot"})
			if err != nil {
				t.Fatal(err)
			}
			req.FrameID = r.Frame.ID
			_, err = c.Run(ctx, req)
			if (err != nil) != (outcome == "unknown") {
				t.Fatal(err)
			}
			if calls != 2 || locks != 1 {
				t.Fatalf("unexpected replay/lease count: %d/%d", calls, locks)
			}
			if _, err := c.Run(ctx, req); err == nil || calls != 2 {
				t.Fatal("reused consumed frame")
			}
			c.Close()
			c.Close()
			if _, err := lease.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("lease not released")
			}
			if _, err := c.Run(ctx, Request{Operation: "screenshot"}); err == nil || calls != 2 {
				t.Fatal("closed controller ran")
			}
		})
	}
}

func TestActionBatchValidatedBeforeAnySideEffects(t *testing.T) {
	frame := testFrame()
	for _, invalid := range []Action{
		{Type: "click", Position: []float64{1000, 0}}, {Type: "move", Position: []float64{-1, 0}},
		{Type: "drag", Position: []float64{1, 1}}, {Type: "scroll", DeltaY: 2001},
		{Type: "type", Text: strings.Repeat("x", 4097)}, {Type: "key", Key: "cmd+unknown"},
		{Type: "wait", Milliseconds: 1001}, {Type: "shell", Text: "whoami"},
	} {
		calls := 0
		c := &Controller{frame: &frame, call: func(context.Context, Request, *os.File) (Response, error) { calls++; return Response{}, nil }}
		_, err := c.Run(context.Background(), Request{Operation: "act", FrameID: frame.ID, Actions: []Action{{Type: "key", Key: "enter"}, invalid}})
		if err == nil || calls != 0 {
			t.Fatalf("invalid batch executed: %+v", invalid)
		}
	}
	frame.CapturedAt = time.Now().Add(-3 * time.Minute)
	c := &Controller{frame: &frame}
	if _, err := c.Run(context.Background(), Request{Operation: "act", FrameID: frame.ID, Actions: []Action{{Type: "key", Key: "enter"}}}); err == nil {
		t.Fatal("expired frame accepted")
	}
}

func TestWorkerCoordinatesPermissionsAndCancellation(t *testing.T) {
	for _, scenario := range []string{"ok", "permission", "display", "cancel", "capture"} {
		t.Run(scenario, func(t *testing.T) {
			f := testFrame()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var actions []Action
			captures := 0
			native := func(req map[string]any) Response {
				switch req["operation"] {
				case "status":
					s := Status{Supported: true, ScreenRecording: true, Accessibility: scenario != "permission", Display: f.Display}
					if scenario == "display" {
						s.Display.Width++
					}
					return Response{Status: &s}
				case "action":
					actions = append(actions, req["action"].(Action))
					if scenario == "cancel" {
						cancel()
					}
					return Response{}
				case "screenshot":
					captures++
					if scenario == "capture" {
						return Response{Error: "capture failed"}
					}
					return Response{Frame: &f, Image: []byte("jpeg")}
				}
				t.Fatalf("unexpected native operation: %v", req)
				return Response{}
			}
			r := runWorker(ctx, Request{Operation: "act", Frame: &f, Actions: []Action{{Type: "click", Position: []float64{500, 250}}, {Type: "type", Text: "中文🙂"}}}, native)
			if scenario == "permission" || scenario == "display" {
				if len(actions) != 0 || captures != 0 || r.Error == "" {
					t.Fatalf("ran with no authority: %+v", r)
				}
				return
			}
			if !reflect.DeepEqual(actions[0].Position, []float64{800, 600}) {
				t.Fatalf("wrong coordinate transform: %+v", actions[0])
			}
			if scenario == "cancel" {
				if len(actions) != 1 || r.Completed != 1 || captures != 0 || r.Error == "" {
					t.Fatalf("cancelled batch continued: %+v", r)
				}
				return
			}
			if len(actions) != 2 || r.Completed != 2 || captures != 1 {
				t.Fatalf("wrong batch outcome: %+v", r)
			}
			if scenario == "capture" && !strings.Contains(r.Error, "do not repeat") {
				t.Fatal("lost partial completion warning")
			}
		})
	}
}

func TestDesktopLeaseExclusiveAndReleased(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	release, err := lockDesktop()
	if err != nil {
		t.Fatal(err)
	}
	defer release.Close()
	if other, err := lockDesktop(); err == nil {
		other.Close()
		t.Fatal("two sessions own desktop")
	}
	release.Close()
	other, err := lockDesktop()
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}

func TestWorkerStatusDoesNotPromptOrCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := callWorker(ctx, Request{Operation: "status"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status == nil || r.Frame != nil || len(r.Image) != 0 || r.Completed != 0 {
		t.Fatalf("invalid read-only probe: %+v", r)
	}
}

func TestDesktopLeaseSurvivesParentDescriptorClose(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lease, err := lockDesktop()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/cat")
	cmd.ExtraFiles = []*os.File{lease}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if other, err := lockDesktop(); err == nil {
		other.Close()
		t.Error("worker lost the desktop lease when parent closed its descriptor")
	}
	input.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	other, err := lockDesktop()
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}
