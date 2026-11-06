// Package computer owns the explicitly enabled desktop capability. It does not
// share the project executor or confer desktop access on shell commands.
package computer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"ccdp/internal/runtimepath"
)

const MaxImageBytes = 2 << 20

type Display struct {
	ID     uint32  `json:"id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type Status struct {
	Supported       bool    `json:"supported"`
	ScreenRecording bool    `json:"screen_recording"`
	Accessibility   bool    `json:"accessibility"`
	Display         Display `json:"display"`
}

func (s Status) Check() error {
	if !s.Supported {
		return errors.New("Computer requires macOS 14 or later and a build with CGO enabled")
	}
	if !s.ScreenRecording || !s.Accessibility {
		return errors.New("Computer needs Screen Recording and Accessibility access in System Settings > Privacy & Security for the app launching ccdp. Grant access, then restart ccdp --computer-use; do not retry tools until access changes")
	}
	return nil
}

type Frame struct {
	ID         string    `json:"id"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	Display    Display   `json:"display"`
	CapturedAt time.Time `json:"captured_at"`
}

type Action struct {
	Type         string    `json:"type"`
	Position     []float64 `json:"position,omitempty"`
	To           []float64 `json:"to,omitempty"`
	Button       string    `json:"button,omitempty"`
	Clicks       int       `json:"clicks,omitempty"`
	Text         string    `json:"text,omitempty"`
	Key          string    `json:"key,omitempty"`
	DeltaX       int       `json:"delta_x,omitempty"`
	DeltaY       int       `json:"delta_y,omitempty"`
	Milliseconds int       `json:"milliseconds,omitempty"`
}

type Request struct {
	Operation string   `json:"operation"`
	FrameID   string   `json:"frame_id,omitempty"`
	Actions   []Action `json:"actions,omitempty"`
	// Frame is supplied by the controller, never accepted from tool arguments.
	Frame *Frame `json:"frame,omitempty"`
}

type Response struct {
	Status    *Status `json:"status,omitempty"`
	Frame     *Frame  `json:"frame,omitempty"`
	Completed int     `json:"completed"`
	Error     string  `json:"error,omitempty"`
	Image     []byte  `json:"image,omitempty"`
}

// Controller holds the latest observation and a desktop lease for one session.
// Stale frames and cancelled/uncertain actions are never replayed automatically.
type Controller struct {
	mu      sync.Mutex
	frame   *Frame
	closed  bool
	lease   *os.File
	call    func(context.Context, Request, *os.File) (Response, error)
	acquire func() (*os.File, error)
}

func NewController() *Controller { return &Controller{call: callWorker, acquire: lockDesktop} }

func (c *Controller) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.frame = nil
	if c.lease != nil {
		_ = c.lease.Close()
		c.lease = nil
	}
}

func (c *Controller) Run(ctx context.Context, req Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Response{}, errors.New("Computer session is closed")
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if req.Frame != nil {
		return Response{}, errors.New("frame is controller-owned; use frame_id")
	}
	if req.Operation != "status" && req.Operation != "screenshot" && req.Operation != "act" {
		return Response{}, errors.New("operation must be status, screenshot, or act")
	}
	if req.Operation == "act" {
		if c.frame == nil || req.FrameID != c.frame.ID || time.Since(c.frame.CapturedAt) > 2*time.Minute {
			return Response{}, errors.New("missing, stale, or consumed frame_id; take a fresh screenshot before acting")
		}
		req.Frame = c.frame
		if err := validateActions(req.Actions, *req.Frame); err != nil {
			return Response{}, err
		}
	} else if len(req.Actions) != 0 || req.FrameID != "" {
		return Response{}, errors.New("actions and frame_id are only valid with operation=act")
	}
	if req.Operation != "status" && c.lease == nil {
		lease, err := c.acquire()
		if err != nil {
			return Response{}, err
		}
		c.lease = lease
	}
	if req.Operation != "status" {
		c.frame = nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.call(ctx, req, c.lease)
	if err != nil {
		return resp, err
	}
	if resp.Frame != nil {
		f := resp.Frame
		if f.Width < 1 || f.Height < 1 || f.Width > 1280 || f.Height > 1280 || f.Display.Width <= 0 || f.Display.Height <= 0 || len(resp.Image) == 0 || len(resp.Image) > MaxImageBytes {
			return Response{}, errors.New("invalid desktop screenshot; inspect the screen before repeating any actions")
		}
		f.ID, err = runtimepath.ID()
		if err != nil {
			return Response{}, err
		}
		f.CapturedAt = time.Now().UTC()
		copy := *f
		c.frame = &copy
	}
	return resp, nil
}

func validateActions(actions []Action, f Frame) error {
	if len(actions) < 1 || len(actions) > 8 {
		return errors.New("act requires 1–8 actions")
	}
	point := func(p []float64) bool {
		return len(p) == 2 && !math.IsNaN(p[0]) && !math.IsNaN(p[1]) && p[0] >= 0 && p[1] >= 0 && p[0] < float64(f.Width) && p[1] < float64(f.Height)
	}
	for i, a := range actions {
		var valid bool
		if (len(a.Position) > 0 && !point(a.Position)) || (len(a.To) > 0 && !point(a.To)) {
			return fmt.Errorf("invalid coordinates in Computer action %d; no actions were executed", i+1)
		}
		switch a.Type {
		case "click":
			valid = point(a.Position) && (a.Button == "" || a.Button == "left" || a.Button == "right") && a.Clicks >= 0 && a.Clicks <= 2
		case "move":
			valid = point(a.Position)
		case "drag":
			valid = point(a.Position) && point(a.To) && (a.Button == "" || a.Button == "left")
		case "scroll":
			valid = a.DeltaX >= -2000 && a.DeltaX <= 2000 && a.DeltaY >= -2000 && a.DeltaY <= 2000 && (a.DeltaX != 0 || a.DeltaY != 0)
		case "type":
			valid = a.Text != "" && len(a.Text) <= 4096 && utf8.ValidString(a.Text)
		case "key":
			_, _, err := parseKey(a.Key)
			valid = err == nil
		case "wait":
			valid = a.Milliseconds >= 1 && a.Milliseconds <= 1000
		}
		if !valid {
			return fmt.Errorf("invalid Computer action %d (%s); no actions were executed", i+1, a.Type)
		}
	}
	return nil
}
