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
)

type ProcessTool struct{}

func NewProcessTool() *ProcessTool { return &ProcessTool{} }
func (*ProcessTool) Name() string  { return "Process" }
func (*ProcessTool) Description() string {
	return "Read output from an existing Bash process by byte offset without consuming it. Continue with id=process_id, action=read, offset=next_offset; add wait_ms to wait for new output. Write stdin when started with stdin=pipe or tty=true (eof closes pipe input), or stop and release the handle. Waiting never reruns a command. log_path is an absolute path readable by Read (which uses line offsets). Logs retain at most 32 MiB; loss is reported in warning. Only the latest 32 completed handles and temporary logs are retained; stop or session close deletes logs. Save lasting results as files."
}
func (*ProcessTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"id": map[string]any{"type": "integer"}, "action": map[string]any{"type": "string", "enum": []string{"read", "write", "stop"}}, "offset": map[string]any{"default": 0, "type": "integer", "minimum": 0}, "limit_bytes": map[string]any{"default": 51200, "type": "integer", "minimum": 4, "maximum": 51200}, "wait_ms": map[string]any{"default": 0, "type": "integer", "minimum": 0, "maximum": 10000}, "input": map[string]any{"type": "string"}, "eof": map[string]any{"default": false, "type": "boolean"}}, "required": []string{"id", "action"}}
}

type processView struct {
	Truncated bool   `json:"truncated,omitempty"`
	ID        int    `json:"process_id"`
	Status    string `json:"status"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Output    string `json:"output"`
	Offset    int    `json:"offset"`
	Next      int    `json:"next_offset"`
	Log       string `json:"log_path"`
	Warning   string `json:"warning,omitempty"`
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
	yield, e := boundedInt(ctx.Args, "yield_ms", 1000, 0, 10000)
	if e != nil {
		return "", e
	}
	timeout := ctx.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if raw := ctx.Args["timeout_ms"]; raw != nil {
		milliseconds, err := boundedInt(ctx.Args, "timeout_ms", 0, 0, 3600000)
		if err != nil {
			return "", err
		}
		timeout = time.Duration(milliseconds) * time.Millisecond
	}
	dir := ctx.WorkingDir
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
	id, mp, e := m.startManaged(owner, command, dir, ctx.Sandbox, tty, input == "pipe", timeout)
	if e != nil {
		return "", e
	}
	limit := 51200
	out, err := mp.view(ctx, id, 0, limit, yield)
	if err != nil {
		// The session owns this process as soon as startManaged succeeds.
		// Preserve its handle when a code cell/turn cancels the initial wait.
		out, _ = mp.view(ctx, id, 0, limit, 0)
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
		id, e := IntArgChecked(ctx.Args, "id", 0)
		if e != nil {
			return "", e
		}
		mp, e := m.Get(id)
		if e != nil {
			return "", e
		}
		switch StringArg(ctx.Args, "action", "") {
		case "read":
			offset, e := boundedInt(ctx.Args, "offset", 0, 0, int(^uint(0)>>1))
			if e != nil {
				return "", e
			}
			limit, e := boundedInt(ctx.Args, "limit_bytes", 51200, 4, 51200)
			if e != nil {
				return "", e
			}
			wait, e := boundedInt(ctx.Args, "wait_ms", 0, 0, 10000)
			if e != nil {
				return "", e
			}
			return mp.view(ctx, id, offset, limit, wait)
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
			data, err := json.Marshal(map[string]any{"process_id": id, "status": "input_sent", "bytes_sent": len(input), "eof": eof})
			return string(data), err
		case "stop":
			_, exited, err := mp.stop()
			if !exited {
				return "", fmt.Errorf("Process: process %d did not exit after stop: %v", id, err)
			}
			output, viewErr := mp.viewResult(ctx, id, 0, 51200, 0, true)
			if viewErr == nil {
				m.Remove(id)
			}
			return output, viewErr
		default:
			return "", fmt.Errorf("Process: action must be read, write, or stop")
		}
	})
	return processResult(result), err
}
func (mp *managedProcess) view(ctx *Context, id, offset, limit, wait int) (string, error) {
	return mp.viewResult(ctx, id, offset, limit, wait, false)
}

// Releasing views describe the state after the caller removes the handle/log,
// rather than advertising a continuation that cannot work once stop returns.
func (mp *managedProcess) viewResult(ctx *Context, id, offset, limit, wait int, releasing bool) (string, error) {
	call := ctx.Context
	if call == nil {
		call = context.Background()
	}
	timer := time.NewTimer(time.Duration(wait) * time.Millisecond)
	defer timer.Stop()
	for {
		mp.mu.Lock()
		total, exited, ch := mp.total, mp.exited, mp.changed
		mp.mu.Unlock()
		if int64(offset) > total {
			return "", fmt.Errorf("Process: offset %d exceeds output end %d", offset, total)
		}
		if int64(offset) < total || exited || wait == 0 {
			break
		}
		select {
		case <-ch:
		case <-mp.done:
		case <-timer.C:
			wait = 0
		case <-call.Done():
			return "", call.Err()
		}
	}
	mp.mu.Lock()
	v := processView{ID: id, Status: "running", Offset: offset, Next: offset, Log: mp.logPath}
	size, total := mp.logSize, mp.total
	exited, perr, reason := mp.exited, mp.err, mp.reason
	logErr := mp.logErr
	tail := string(mp.buf)
	mp.mu.Unlock()
	if exited {
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
		v.Warning = baseWarning
		if releasing {
			v.Warning += " Process handle and temporary log released; any output omitted from this response is no longer available through Process or log_path."
		} else if int64(v.Next) < size {
			v.Warning += fmt.Sprintf(" More retained output: use Process id=%d action=read offset=%d (bytes); or Read file_path=%q offset=1 (lines).", id, v.Next, v.Log)
		}
		data, err := json.Marshal(v)
		return data, n, err
	}
	budget := ctx.outputLimit()
	data, _, err := encode(len(raw))
	if err != nil || len(data) <= budget {
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
