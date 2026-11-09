package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"ccdp/internal/protocol"
)

type ProcessTool struct{}

func NewProcessTool() *ProcessTool { return &ProcessTool{} }
func (*ProcessTool) Name() string  { return "Process" }
func (*ProcessTool) Description() string {
	return "Inspect or control an owned background command using its complete id. read returns current output with byte offsets. wait blocks until completion with UI progress; completion is also delivered automatically, so do not poll. write sends stdin (eof closes pipe input); stop terminates and releases the handle. Cancelling an observation leaves the command running. After completion, use ReadOutput with its output_id for retained logs."
}
func (*ProcessTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"id":          map[string]any{"type": "string", "minLength": 1},
		"action":      map[string]any{"type": "string", "enum": []string{"read", "wait", "write", "stop"}},
		"offset":      map[string]any{"type": "integer", "minimum": 0, "description": "Only for read; raw log byte offset."},
		"limit_bytes": map[string]any{"type": "integer", "minimum": 4, "maximum": 51200, "description": "Only for read."},
		"input":       map[string]any{"type": "string", "description": "Only for write."}, "eof": map[string]any{"type": "boolean", "description": "Only for write; close pipe input."},
	}, "required": []string{"id", "action"}}
}

func startCommand(ctx *Context) (string, error) {
	if ctx.Context != nil && ctx.Context.Err() != nil {
		return "", ctx.Context.Err()
	}
	if e := ctx.checkResources(); e != nil {
		return "", e
	}
	command := StringArg(ctx.Args, "command", "")
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("Bash: command required")
	}
	background := BoolArg(ctx.Args, "background", false) || BoolArg(ctx.Args, "tty", false) || StringArg(ctx.Args, "stdin", "closed") == "pipe"
	timeout := ctx.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if background {
		timeout = 0
	}
	if raw := ctx.Args["timeout_ms"]; raw != nil {
		milliseconds, err := boundedInt(ctx.Args, "timeout_ms", 0, 0, 3600000)
		if err != nil {
			return "", err
		}
		timeout = time.Duration(milliseconds) * time.Millisecond
	}
	dir := ctx.WorkingDir
	var e error
	if raw := StringArg(ctx.Args, "cwd", ""); raw != "" {
		dir, e = ctx.ResolveRead(raw)
		if e != nil {
			return "", e
		}
	}
	m, e := ctx.processManager()
	if e != nil {
		return "", e
	}
	owner := context.Background()
	if ctx.Resources != nil {
		owner = ctx.Resources.OwnerContext()
	}
	tty := BoolArg(ctx.Args, "tty", false)
	input := StringArg(ctx.Args, "stdin", "closed")
	if input != "closed" && input != "pipe" {
		return "", fmt.Errorf("Bash: stdin must be closed or pipe")
	}
	if tty && input == "pipe" {
		return "", fmt.Errorf("Bash: tty=true already supplies terminal input; do not combine with stdin=pipe")
	}
	id, mp, e := m.startManaged(owner, command, dir, ctx.Sandbox, tty, input == "pipe", timeout, &commandTracking{info: protocol.ProcessInfo{Owner: m.owner, Generation: m.generation, Origin: ctx.ProcessOrigin, Command: truncateUTF8(command, 2000), Description: truncateUTF8(StringArg(ctx.Args, "description", ""), 320), Background: background, Cwd: dir, Environment: ctx.Resources.environmentDir}, events: ctx.ProcessEvent})
	if e != nil {
		return "", e
	}
	if !background {
		if err := mp.waitForeground(ctx); err != nil {
			_, _, stopErr := mp.stop()
			out, _ := mp.view(ctx, id, 0, 8192)
			if stopErr != nil {
				return out, fmt.Errorf("%w; stopping command: %v", err, stopErr)
			}
			return out, err
		}
	}
	out, err := mp.view(ctx, id, 0, 8192)
	if err != nil {
		// Never leave a command running when its initial handle cannot be
		// delivered (for example, an output budget smaller than the metadata).
		_, exited, stopErr := mp.stop()
		if exited {
			m.Remove(id)
		}
		if stopErr != nil {
			return "", fmt.Errorf("Bash: process %s started but its result could not be delivered: %w; stopping command: %v", id, err, stopErr)
		}
		return "", fmt.Errorf("Bash: process %s started, then was stopped and released because its result could not be delivered: %w", id, err)
	}
	return out, err
}
func boundedInt(args map[string]any, key string, def, min, max int) (int, error) {
	n, e := IntArgChecked(args, key, def)
	if e != nil {
		return 0, e
	}
	if n < min || n > max {
		return 0, fmt.Errorf("%s must be %d..%d", key, min, max)
	}
	return n, nil
}
func (*ProcessTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	result, err := executeText(ctx, args, func(ctx *Context) (string, error) {
		m, e := ctx.processManager()
		if e != nil {
			return "", e
		}
		id, ok := ctx.Args["id"].(string)
		if !ok || id == "" {
			return "", fmt.Errorf("Process: id must be a nonempty string")
		}
		action := StringArg(ctx.Args, "action", "")
		allowed := map[string]bool{"id": true, "action": true}
		if action == "read" {
			allowed["offset"], allowed["limit_bytes"] = true, true
		}
		if action == "write" {
			allowed["input"], allowed["eof"] = true, true
		}
		for key := range ctx.Args {
			if !allowed[key] {
				return "", fmt.Errorf("Process: %s is not valid for action %s", key, action)
			}
		}
		mp, e := m.Get(id)
		if e != nil {
			return "", e
		}
		switch StringArg(ctx.Args, "action", "") {
		case "wait":
			if err := mp.waitForeground(ctx); err != nil {
				return "", err
			}
			out, err := mp.view(ctx, id, 0, 8192)
			return out, err
		case "read":
			offset, e := boundedInt(ctx.Args, "offset", 0, 0, int(^uint(0)>>1))
			if e != nil {
				return "", e
			}
			limit, e := boundedInt(ctx.Args, "limit_bytes", 8192, 4, 51200)
			if e != nil {
				return "", e
			}
			return mp.view(ctx, id, offset, limit)
		case "write":
			input, ok := ctx.Args["input"].(string)
			eof := BoolArg(ctx.Args, "eof", false)
			if !ok && !eof {
				return "", fmt.Errorf("Process: input or eof=true required")
			}
			if len(input) > ctx.processInputLimit() {
				return "", fmt.Errorf("Process: input exceeds limit")
			}
			if ok {
				if e = mp.write(input); e != nil {
					return "", e
				}
			}
			if eof {
				e = mp.cmd.CloseInput()
				if e != nil {
					return "", e
				}
			}
			view := protocol.ProcessOutput{ID: id, Status: "input_sent", BytesSent: len(input), EOF: eof}
			ctx.recordProcess(view, false)
			data, err := json.Marshal(view)
			return string(data), err
		case "stop":
			_, exited, err := mp.stop()
			if !exited {
				return "", fmt.Errorf("Process: process %s did not exit after stop: %v", id, err)
			}
			output, viewErr := mp.viewResult(ctx, id, 0, 8192, true)
			if viewErr == nil {
				m.Remove(id)
			}
			return output, viewErr
		default:
			return "", fmt.Errorf("Process: action must be read, wait, write, or stop")
		}
	})
	return processResult(result), err
}
func (mp *managedProcess) view(ctx *Context, id string, offset, limit int) (string, error) {
	return mp.viewResult(ctx, id, offset, limit, false)
}

// Releasing views describe the state after the caller removes the handle/log,
// rather than advertising a continuation that cannot work once stop returns.
func (mp *managedProcess) viewResult(ctx *Context, id string, offset, limit int, releasing bool) (string, error) {
	call := ctx.Context
	if call == nil {
		call = context.Background()
	}
	if err := call.Err(); err != nil {
		return "", err
	}
	mp.mu.Lock()
	if int64(offset) > mp.total {
		total := mp.total
		mp.mu.Unlock()
		return "", fmt.Errorf("Process: offset %d exceeds output end %d", offset, total)
	}
	v := mp.identity.ProcessOutput
	v.ID, v.Status, v.Offset, v.Next, v.Log = id, "running", offset, offset, mp.logPath
	size, total := mp.logSize, mp.total
	exited, perr, reason := false, mp.err, mp.reason
	select {
	case <-mp.done:
		exited = true
	default:
	}
	logErr := mp.logErr
	tail := string(mp.tail.Tail(2048))
	mp.mu.Unlock()
	if exited {
		v.ResultID = "result." + id
		code := 0
		if perr != nil {
			code = 1
			type exitCoder interface{ ExitCode() int }
			if x, ok := perr.(exitCoder); ok {
				code = x.ExitCode()
			}
		}
		v.ExitCode = &code
		v.Status = "exited"
		if reason != "" {
			v.Status = reason
		}
	}
	if logErr != nil {
		v.Warning = "log write failed: " + logErr.Error()
	}
	if total > size {
		v.Warning += fmt.Sprintf(" Output lost: %d bytes were not retained and cannot be recovered by Process, Read or ReadOutput. Tail preview: ", total-size) + truncateUTF8(tail, 2000)
	}
	var raw []byte
	if int64(offset) < size {
		f, e := os.Open(v.Log)
		if e != nil {
			return "", e
		}
		defer f.Close()
		buf := make([]byte, min(int64(limit), size-int64(offset)))
		n, e := f.ReadAt(buf, int64(offset))
		if e != nil && e != io.EOF {
			return "", e
		}
		buf = buf[:n]
		for len(buf) > 0 && buf[0]&0xc0 == 0x80 {
			buf = buf[1:]
			v.Offset++
		}
		// Defer only an incomplete final codepoint. Invalid interior bytes are
		// consumed and visibly replaced, so a corrupt log cannot stall reads.
		consumed := 0
		for consumed < len(buf) {
			if !utf8.FullRune(buf[consumed:]) && (int64(offset+n) < size || !exited && size == total && size < maxProcessLog && logErr == nil) {
				break
			}
			_, width := utf8.DecodeRune(buf[consumed:])
			consumed += width
		}
		buf = buf[:consumed]
		if v.Offset != offset {
			v.Warning += " Offset adjusted to a UTF-8 character boundary."
		}
		raw = buf
	}
	if int64(offset) >= size && int64(offset) < total {
		v.Next = int(total)
	}
	// Size the actual JSON, not the worst-case six-byte escape expansion.
	// Ordinary logs should use the available budget; escaped output must still
	// preserve a valid envelope and a byte cursor into the original log.
	baseWarning := v.Warning
	if releasing {
		v.Log = ""
	}
	encode := func(n int) ([]byte, int, error) {
		for n > 0 && n < len(raw) && !utf8.RuneStart(raw[n]) {
			n--
		}
		v.Output = strings.ToValidUTF8(string(raw[:n]), "�")
		if raw != nil {
			v.Next = v.Offset + n
		}
		v.Truncated = int64(v.Next) < total || size < total
		v.Tail = ""
		if exited && perr != nil && offset == 0 && v.Truncated {
			n := min(2048, len(tail), int(total)-v.Next)
			tailBytes := tail[len(tail)-n:]
			for len(tailBytes) > 0 && tailBytes[0]&0xc0 == 0x80 {
				tailBytes = tailBytes[1:]
			}
			v.Tail = strings.ToValidUTF8(tailBytes, "�")
		}
		v.Warning = baseWarning
		if releasing && v.OutputID == "" {
			v.Warning += " Process handle and temporary log released; any output omitted from this response is no longer available through Process or log_path."
		}
		data, err := json.Marshal(v)
		return data, n, err
	}
	record := func() {
		ctx.recordProcess(v, v.Terminal() && StringArg(ctx.Args, "action", "") != "read" && StringArg(ctx.Args, "action", "") != "write")
	}
	budget := ctx.outputLimit()
	data, _, err := encode(len(raw))
	if err != nil || len(data) <= budget {
		if err == nil {
			record()
		}
		return string(data), err
	}
	low, high := 0, len(raw)
	for low < high {
		mid := low + (high-low+1)/2
		candidate, _, err := encode(mid)
		if err != nil {
			return "", err
		}
		if len(candidate) <= budget {
			low = mid
		} else {
			high = mid - 1
		}
	}
	data, consumed, err := encode(low)
	if len(data) > budget || len(raw) > 0 && consumed == 0 {
		return "", fmt.Errorf("Process: output budget too small for metadata and output")
	}
	record()
	return string(data), err
}
func truncateUTF8(s string, n int) string {
	if len(s) > n {
		s = s[:n]
		for !utf8.ValidString(s) && len(s) > 0 {
			s = s[:len(s)-1]
		}
	}
	return s
}
