package protocol

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// ProcessCompletionLabel labels result data; no state is parsed from it.
const ProcessCompletionLabel = "Command completed (runtime data, not user input or approval):\n"

// ProcessOutput is the model/program payload. Display metadata lives in ProcessState.
type ProcessOutput struct {
	ResultID  string `json:"result_id,omitempty"`
	OutputID  string `json:"output_id,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	ID        string `json:"process_id"`
	Status    string `json:"status"`
	BytesSent int    `json:"bytes_sent,omitempty"`
	EOF       bool   `json:"eof,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Output    string `json:"output"`
	Tail      string `json:"output_tail,omitempty"`
	Offset    int    `json:"offset"`
	Next      int    `json:"next_offset"`
	Log       string `json:"log_path"`
	Warning   string `json:"warning,omitempty"`
}

func (p ProcessOutput) Terminal() bool {
	return p.Status != "" && p.Status != "registered" && p.Status != "running" && p.Status != "input_sent"
}

func CloneProcessOutput(p *ProcessOutput) *ProcessOutput {
	if p == nil {
		return nil
	}
	c := *p
	if p.ExitCode != nil {
		code := *p.ExitCode
		c.ExitCode = &code
	}
	return &c
}

// ProcessInfo is display and execution metadata, never repeated in model output.
type ProcessInfo struct {
	Owner       string `json:"owner_session_id"`
	Generation  string `json:"executor_generation"`
	Origin      string `json:"origin_tool_id"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	Background  bool   `json:"background,omitempty"`
	Cwd         string `json:"cwd"`
	Environment string `json:"environment"`
}

type ProcessState struct {
	ProcessOutput
	ProcessInfo
	Revision   uint64    `json:"revision"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Released   bool      `json:"released,omitempty"`
}

func CloneProcessState(p *ProcessState) *ProcessState {
	if p == nil {
		return nil
	}
	c := *p
	c.ProcessOutput = *CloneProcessOutput(&p.ProcessOutput)
	return &c
}

// ProcessEvent travels from the command owner to the session owner. The latter
// acknowledges durable admission before dispatch, and completion before wait returns.
type ProcessEvent struct {
	Kind  string
	State ProcessState
	// Output is the owned, completed log. Its descriptor remains open until
	// durable completion is acknowledged; model-supplied paths are never read.
	Output io.Reader
}

func (p ProcessOutput) Reference() ProcessOutput {
	p.Output, p.Tail, p.Warning = "", "", ""
	p.Truncated = true
	return p
}

// Text exposes only actionable metadata. Full state remains available to
// programs and the UI without repeating opaque IDs for completed commands.
func (p ProcessOutput) Text() string {
	var out strings.Builder
	switch {
	case p.Status == "input_sent":
		fmt.Fprintf(&out, "Sent %d bytes; process_id=%s", p.BytesSent, p.ID)
		if p.EOF {
			out.WriteString("; stdin closed")
		}
	case !p.Terminal():
		fmt.Fprintf(&out, "Running; process_id=%s", p.ID)
	case p.ExitCode != nil:
		fmt.Fprintf(&out, "Exit code: %d", *p.ExitCode)
		if p.Status != "exited" {
			fmt.Fprintf(&out, " (%s)", p.Status)
		}
	default:
		fmt.Fprintf(&out, "Status: %s", p.Status)
	}
	if p.Output != "" {
		out.WriteString("\n" + p.Output)
	}
	if p.Tail != "" {
		out.WriteString("\n[final output]\n" + p.Tail)
	}
	if p.Warning != "" {
		out.WriteString("\n" + strings.TrimSpace(p.Warning))
	}
	if p.Truncated {
		switch {
		case p.OutputID != "":
			fmt.Fprintf(&out, "\n[incomplete; ReadOutput output_id=%q offset=%d]", p.OutputID, p.Next)
		case p.Log != "":
			fmt.Fprintf(&out, "\n[incomplete; Process id=%q action=read offset=%d]", p.ID, p.Next)
		default:
			out.WriteString("\n[incomplete; no retained output available]")
		}
	}
	return out.String()
}

// FitProcessOutput preserves the completion and its result reference. An
// empty string means even metadata cannot fit, so delivery must stay pending.
func FitProcessOutput(p ProcessOutput, limit int) (ProcessOutput, string) {
	if limit <= 0 {
		return p, ""
	}
	text := p.Text()
	if len(text) <= limit {
		return p, text
	}
	// This is a second budget boundary, after the tool computed its raw byte
	// cursor. Never cut decoded text and pretend its UTF-8 length is a raw
	// log offset. Keep a reference and an unconsumed cursor instead.
	p.Output, p.Tail, p.Warning = "", "", ""
	p.Next = p.Offset
	p.Truncated = true
	text = p.Text()
	if len(text) > limit {
		return p, ""
	}
	return p, text
}

// ProcessProgress describes raw log byte positions, independently of display
// text (which can replace invalid UTF-8 and include an omission marker).
type ProcessProgress struct {
	ID        string `json:"process_id"`
	Offset    int64  `json:"offset"`
	Next      int64  `json:"next_offset"`
	Truncated bool   `json:"truncated,omitempty"`
}

func CloneProcessProgress(p *ProcessProgress) *ProcessProgress {
	if p == nil {
		return nil
	}
	q := *p
	return &q
}
