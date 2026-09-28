package workspace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/fsops"
	"ccdp/internal/sandbox"
)

type LanguageServer struct {
	Argv       []string `json:"argv"`
	Extensions []string `json:"extensions"`
	LanguageID string   `json:"language_id"`
}
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type Location struct {
	Name  string `json:"name,omitempty"`
	URI   string `json:"uri"`
	Range struct {
		Start Position `json:"start"`
		End   Position `json:"end"`
	} `json:"range"`
}

// LanguageQuery owns a bounded stdio LSP session. It never auto-installs or
// guesses executables. The caller authorizes the configured command first.
func LanguageQuery(ctx context.Context, root, path, operation string, line, character int, server LanguageServer, policy *sandbox.Sandbox) ([]Location, error) {
	if line < 1 || character < 0 {
		return nil, errors.New("line is one-based; character is zero-based UTF-16")
	}
	if len(server.Argv) == 0 {
		return nil, errors.New("no configured language server")
	}
	if strings.TrimSpace(server.LanguageID) == "" {
		return nil, errors.New("language server requires language_id")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	resolved, e := policy.ResolveRead(path)
	if e != nil {
		return nil, e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return nil, e
	}
	resolved, e = filepath.EvalSymlinks(resolved)
	if e != nil {
		return nil, e
	}
	rel, e := filepath.Rel(root, resolved)
	if e != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, errors.New("LSP document must be inside workspace")
	}
	f, e := fsops.OpenRegular(root, resolved)
	if e != nil {
		return nil, e
	}
	data, e := io.ReadAll(io.LimitReader(f, 2<<20+1))
	_ = f.Close()
	if e != nil {
		return nil, e
	}
	if len(data) > 2<<20 {
		return nil, errors.New("document exceeds LSP budget")
	}
	method := "textDocument/definition"
	if operation == "references" {
		method = "textDocument/references"
	} else if operation == "outline" {
		method = "textDocument/documentSymbol"
	} else if operation != "definition" {
		return nil, errors.New("unsupported LSP operation")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sb := policy.Snapshot()
	sb.AddReadOnlyDir(root)
	sb.AllowNetwork = false
	cmd, e := execution.StartArgv(execution.StartRequest{Context: ctx, Argv: server.Argv, Dir: root, Sandbox: sb, Env: execution.SanitizedEnvironmentFor(execution.EnvironmentGit, os.Environ())})
	if e != nil {
		return nil, e
	}
	defer execution.CleanupStartedProcess(cmd)
	stdin, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	defer stdout.Close()
	cmd.SetStderr(io.Discard)
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	defer func() { cancel(); _ = stdin.Close(); _ = cmd.Wait() }()
	reader := bufio.NewReaderSize(stdout, 8192)
	send := func(v any) error {
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		_, e = fmt.Fprintf(stdin, "Content-Length: %d\r\n\r\n%s", len(b), b)
		return e
	}
	receive := func(want int) (json.RawMessage, error) {
		total := 0
		for count := 0; count < 1000; count++ {
			size := 0
			headers := 0
			for {
				h, err := reader.ReadString('\n')
				if err != nil {
					return nil, err
				}
				headers += len(h)
				if headers > 8192 {
					return nil, errors.New("oversized LSP header")
				}
				h = strings.TrimSpace(h)
				if h == "" {
					break
				}
				k, v, ok := strings.Cut(h, ":")
				if ok && strings.EqualFold(k, "Content-Length") {
					size, err = strconv.Atoi(strings.TrimSpace(v))
					if err != nil {
						return nil, err
					}
				}
			}
			if size <= 0 || size > 8<<20 || total+size > 32<<20 {
				return nil, errors.New("LSP response budget exceeded")
			}
			total += size
			b := make([]byte, size)
			if _, err := io.ReadFull(reader, b); err != nil {
				return nil, err
			}
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(b, &msg); err != nil {
				return nil, err
			}
			if msg.Method != "" {
				if len(msg.ID) > 0 {
					response := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
					if msg.Method == "workspace/configuration" {
						var p struct {
							Items []json.RawMessage `json:"items"`
						}
						_ = json.Unmarshal(msg.Params, &p)
						values := make([]any, len(p.Items))
						response["result"] = values
					} else {
						response["error"] = map[string]any{"code": -32601, "message": "unsupported client request"}
					}
					if err := send(response); err != nil {
						return nil, err
					}
				}
				continue
			}
			var id int
			if json.Unmarshal(msg.ID, &id) != nil || id != want {
				continue
			}
			if len(msg.Error) > 0 && string(msg.Error) != "null" {
				return nil, fmt.Errorf("language server: %s", msg.Error)
			}
			return msg.Result, nil
		}
		return nil, errors.New("too many LSP notifications")
	}
	uri := (&url.URL{Scheme: "file", Path: resolved}).String()
	rootURI := (&url.URL{Scheme: "file", Path: root}).String()
	if e = send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"processId": nil, "rootUri": rootURI, "capabilities": map[string]any{"general": map[string]any{"positionEncodings": []string{"utf-16"}}}}}); e != nil {
		return nil, e
	}
	init, e := receive(1)
	if e != nil {
		return nil, e
	}
	var initialization struct {
		Capabilities struct {
			PositionEncoding string `json:"positionEncoding"`
		} `json:"capabilities"`
	}
	if e = json.Unmarshal(init, &initialization); e != nil {
		return nil, e
	}
	if enc := initialization.Capabilities.PositionEncoding; enc != "" && enc != "utf-16" {
		return nil, errors.New("server selected unsupported position encoding")
	}
	if e = send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); e != nil {
		return nil, e
	}
	if e = send(map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": server.LanguageID, "version": 1, "text": string(data)}}}); e != nil {
		return nil, e
	}
	params := map[string]any{"textDocument": map[string]any{"uri": uri}}
	if operation != "outline" {
		params["position"] = Position{line - 1, character}
	}
	if operation == "references" {
		params["context"] = map[string]any{"includeDeclaration": true}
	}
	if e = send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": method, "params": params}); e != nil {
		return nil, e
	}
	raw, e := receive(2)
	if e != nil {
		return nil, e
	}
	if string(raw) == "null" {
		return []Location{}, nil
	}
	if operation == "outline" {
		type symbol struct {
			Name           string            `json:"name"`
			Location       Location          `json:"location"`
			SelectionRange json.RawMessage   `json:"selectionRange"`
			Children       []json.RawMessage `json:"children"`
		}
		var values []json.RawMessage
		if e = json.Unmarshal(raw, &values); e != nil {
			return nil, e
		}
		var flat []Location
		var visit func([]json.RawMessage) error
		visit = func(rows []json.RawMessage) error {
			for _, row := range rows {
				var s symbol
				if e := json.Unmarshal(row, &s); e != nil {
					return e
				}
				l := s.Location
				l.Name = s.Name
				if l.URI == "" {
					l.URI = uri
					if e := json.Unmarshal(s.SelectionRange, &l.Range); e != nil {
						return e
					}
				}
				flat = append(flat, l)
				if e := visit(s.Children); e != nil {
					return e
				}
			}
			return nil
		}
		if e = visit(values); e != nil {
			return nil, e
		}
		raw, e = json.Marshal(flat)
		if e != nil {
			return nil, e
		}
	}
	var locations []Location
	if e = json.Unmarshal(raw, &locations); e != nil {
		var one Location
		if e = json.Unmarshal(raw, &one); e != nil {
			return nil, e
		}
		locations = []Location{one}
	}
	// Definition may return LocationLink instead of Location.
	if len(locations) > 0 && locations[0].URI == "" {
		var links []struct {
			TargetURI            string          `json:"targetUri"`
			TargetSelectionRange json.RawMessage `json:"targetSelectionRange"`
		}
		if e = json.Unmarshal(raw, &links); e != nil {
			return nil, e
		}
		locations = nil
		for _, l := range links {
			v := Location{URI: l.TargetURI}
			if e = json.Unmarshal(l.TargetSelectionRange, &v.Range); e != nil {
				return nil, e
			}
			locations = append(locations, v)
		}
	}
	out := []Location{}
	for _, l := range locations {
		u, e := url.Parse(l.URI)
		if e != nil || u.Scheme != "file" || u.Host != "" && u.Host != "localhost" {
			continue
		}
		if _, e = policy.ResolveRead(u.Path); e != nil {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}
