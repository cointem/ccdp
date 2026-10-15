package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"ccdp/internal/executable"
	"ccdp/internal/execution"
)

type rgText struct {
	Text  string `json:"text"`
	Bytes string `json:"bytes"`
}

func (t rgText) value() string {
	if t.Bytes != "" {
		b, _ := base64.StdEncoding.DecodeString(t.Bytes)
		return string(b)
	}
	return t.Text
}

type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path         rgText `json:"path"`
		Lines        rgText `json:"lines"`
		Line         int    `json:"line_number"`
		BinaryOffset *int   `json:"binary_offset"`
		Stats        struct {
			MatchedLines int `json:"matched_lines"`
		} `json:"stats"`
		Submatches []struct {
			Match rgText `json:"match"`
		} `json:"submatches"`
	} `json:"data"`
}

func ripgrepSearch(ctx *Context, r *rgRequest) ([]rgLine, bool, error) {
	canonical, err := canonicalRgRequest(ctx, r)
	if err != nil {
		return nil, false, err
	}
	r = canonical
	if executable.FindTrusted("rg", os.Getenv("PATH")) == "" {
		return nil, false, fmt.Errorf("Rg: ripgrep (rg) is required; install it and add its trusted installation directory to PATH (macOS: brew install ripgrep)")
	}
	call := ctx.Context
	if call == nil {
		call = context.Background()
	}
	timeout := rgTimeout
	if ctx.Timeout > 0 {
		timeout = ctx.Timeout
	}
	call, cancel := context.WithTimeout(call, timeout)
	defer cancel()
	local := *ctx
	local.Context = call
	ctx = &local
	limit := r.fetchLines()
	if r.outputMode == rgModeContent || r.outputMode == "" {
		limit *= 1 + r.before + r.after
	}
	var out []rgLine
	truncated := false
	args := []string{"--no-config", "--no-ignore", "--no-mmap", "--color=never", "--encoding=none", "--sort=path", "--json"}
	if r.outputMode != rgModeCount {
		n := r.fetchLines() + 1
		if r.outputMode == rgModeFiles {
			n = 1
		}
		args = append(args, "--max-count", fmt.Sprint(n))
	}
	if r.outputMode == rgModeContent || r.outputMode == "" {
		args = append(args, "--before-context", fmt.Sprint(r.before), "--after-context", fmt.Sprint(r.after))
	}
	if r.ignoreCase {
		args = append(args, "--ignore-case")
	}
	if r.literal {
		args = append(args, "--fixed-strings")
	}
	if r.multiline {
		args = append(args, "--multiline", "--multiline-dotall", "--max-filesize", fmt.Sprint(ctx.readLimit()))
	}
	args = append(args, "--regexp", r.pattern, "--")
	searched := false
	var batch []string
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			if searched {
				return nil
			}
			batch = []string{"-"}
		}
		searched = true
		argv := append(append([]string(nil), args...), batch...)
		parser := &rgOutput{request: r, limit: limit - len(out), maxRecord: 6*ctx.readLimit() + 64<<10}
		res, err := execution.RunReadOnlySearch(call, argv, execution.Request{Dir: r.base, Sandbox: ctx.Sandbox, Timeout: timeout, OutputLimit: 4096}, parser, ctx.ReadRoot)
		batch = nil
		batchBytes = 0
		if err != nil {
			return err
		}
		if call.Err() != nil {
			return call.Err()
		}
		if res.TimedOut {
			return fmt.Errorf("Rg: search timed out")
		}
		if res.ExitCode > 1 {
			return fmt.Errorf("Rg: ripgrep failed: %s", clipLine(res.Stderr))
		}
		if parser.err != nil {
			return parser.err
		}
		out = append(out, parser.out...)
		truncated = truncated || parser.incomplete || len(parser.pending) > 0

		if len(out) > limit || truncated {
			truncated = true
			return fs.SkipAll
		}
		return nil
	}
	err = walkRgFiles(ctx, r, func(path, rel string) error {
		if r.multiline {
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if info.Size() > int64(ctx.readLimit()) {
				truncated = true
				return nil
			}
		}
		// Prefix avoids filenames beginning with '-' being interpreted as options.
		batch = append(batch, "./"+rel)
		batchBytes += len(rel) + 3
		if len(batch) >= 128 || batchBytes >= 32<<10 {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil && err != fs.SkipAll {
		return nil, false, err
	}
	if call.Err() != nil {
		return nil, false, call.Err()
	}
	return out, truncated, nil
}

// rgOutput drains machine output with bounded memory. Per-file results are
// committed only at an end event, so binary detection and incomplete captures
// cannot turn a partial scan into a complete count.
type rgOutput struct {
	request                                *rgRequest
	limit, maxRecord                       int
	pending                                []byte
	discarding, incomplete, fileIncomplete bool
	fileLines, out                         []rgLine
	err                                    error
}

func (p *rgOutput) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		complete := i >= 0
		if !complete {
			i = len(data)
		}
		if !p.discarding {
			if len(p.pending)+i > p.maxRecord {
				p.pending = nil
				p.discarding = true
				p.incomplete = true
				p.fileIncomplete = true
			} else {
				p.pending = append(p.pending, data[:i]...)
			}
		}
		if !complete {
			break
		}
		if !p.discarding {
			p.event(p.pending)
		}
		p.pending = p.pending[:0]
		p.discarding = false
		data = data[i+1:]
	}
	return n, nil
}
func (p *rgOutput) event(raw []byte) {
	if p.err != nil {
		return
	}
	var e rgEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		p.err = fmt.Errorf("Rg: malformed ripgrep output: %w", err)
		return
	}
	d := e.Data
	file := strings.TrimPrefix(d.Path.value(), "./")
	switch e.Type {
	case "begin":
		p.fileLines = nil
		p.fileIncomplete = false
	case "match", "context":
		if p.request.outputMode == rgModeCount || p.request.outputMode == rgModeFiles {
			return
		}
		if len(p.fileLines) > p.limit {
			return
		}
		text := strings.TrimSuffix(d.Lines.value(), "\n")
		if p.request.onlyMatch && e.Type == "match" && len(d.Submatches) > 0 {
			text = d.Submatches[0].Match.value()
		}
		for i, line := range strings.Split(text, "\n") {
			if len(p.fileLines) > p.limit {
				break
			}
			p.fileLines = append(p.fileLines, rgLine{file: file, line: d.Line + i, text: clipLine(strings.TrimSuffix(line, "\r")), match: e.Type == "match"})
		}

	case "end":
		if d.BinaryOffset != nil || p.fileIncomplete {
			return
		}
		if p.request.outputMode == rgModeCount || p.request.outputMode == rgModeFiles {
			if d.Stats.MatchedLines > 0 {
				if p.request.outputMode == rgModeFiles {
					p.out = append(p.out, rgLine{file: file, match: true})
				} else {
					p.out = append(p.out, rgLine{file: file, line: d.Stats.MatchedLines, count: d.Stats.MatchedLines, match: true})
				}
			}
		} else {
			p.out = append(p.out, p.fileLines...)
		}
		if len(p.out) > p.limit {
			p.incomplete = true
			p.out = p.out[:p.limit+1]
		}
	}
}
