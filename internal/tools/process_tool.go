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
	return "Read output from an existing Bash process by byte offset without consuming it; write stdin when started with stdin=pipe or tty=true (eof closes pipe input), or stop and release the handle. Waiting never reruns a command. Only the latest 32 completed handles and temporary logs are retained; stop or session close deletes logs. Save lasting results as files."
}
func (*ProcessTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "integer"}, "action": map[string]any{"type": "string", "enum": []string{"read", "write", "stop"}}, "offset": map[string]any{"type": "integer", "minimum": 0}, "limit_bytes": map[string]any{"type": "integer", "minimum": 4, "maximum": 51200}, "wait_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000}, "input": map[string]any{"type": "string"}, "eof": map[string]any{"type": "boolean"}}, "required": []string{"id", "action"}}
}

type processView struct {
	ID       int    `json:"process_id"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Output   string `json:"output"`
	Offset   int    `json:"offset"`
	Next     int    `json:"next_offset"`
	Log      string `json:"log_path"`
	Warning  string `json:"warning,omitempty"`
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
	timeout, e := boundedInt(ctx.Args, "timeout_ms", 120000, 0, 3600000)
	if e != nil {
		return "", e
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
	id, mp, e := m.startManaged(owner, command, dir, ctx.Sandbox, tty, input == "pipe", time.Duration(timeout)*time.Millisecond)
	if e != nil {
		return "", e
	}
	return mp.view(ctx, id, 0, min(51200, max(4, (ctx.outputLimit()-2048)/6)), yield)
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
func (*ProcessTool) Run(ctx *Context) (string, error) {
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
		return mp.view(ctx, id, offset, min(limit, max(4, (ctx.outputLimit()-2048)/6)), wait)
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
		return fmt.Sprintf("Sent %d bytes; eof=%t", len(input), eof), nil
	case "stop":
		_, exited, err := mp.stop()
		if !exited {
			return "", fmt.Errorf("Process: process %d did not exit after stop: %v", id, err)
		}
		output, viewErr := mp.view(ctx, id, 0, min(51200, max(4, (ctx.outputLimit()-2048)/6)), 0)
		if viewErr == nil {
			m.Remove(id)
		}
		return output, viewErr
	default:
		return "", fmt.Errorf("Process: action must be read, write, or stop")
	}
}
func (mp *managedProcess) view(ctx *Context, id, offset, limit, wait int) (string, error) {
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
		v.Warning += " Output exceeded retained log; later bytes are unavailable. Tail: " + truncateUTF8(tail, 2000)
	}
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
		v.Output = strings.ToValidUTF8(string(buf), "�")
		v.Next = v.Offset + len(buf)
	}
	if int64(offset) >= size && int64(offset) < total {
		v.Next = int(total)
	}
	data, e := json.Marshal(v)
	return string(data), e
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
