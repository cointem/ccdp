package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/tools"
)

// fakeServerEnv triggers the in-process fake MCP server (used by the
// subprocess test below).
const fakeServerEnv = "CCDP_TEST_MCP_FAKE_SERVER"

// runFakeServer serves a minimal MCP server over stdio from within the test
// binary itself, so the tests exercise the real process spawning + handshake +
// tools/list + tools/call path.
func runFakeServer() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil || len(msg.ID) == 0 {
			continue // ignore notifications
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name":        "echo",
				"description": "Echo the text argument",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
				},
			}}}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			text := fmt.Sprintf("echo:%v", params.Arguments["text"])
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": false,
			}
		default:
			result = map[string]any{}
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		os.Stdout.Write(append(resp, '\n'))
	}
	os.Exit(0)
}

// newTestServer spawns the fake server with the given tool name.
func newTestServer(t *testing.T, name string) *Client {
	t.Helper()
	cfg := ServerConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestClientLifecycle"},
		Env:     map[string]string{fakeServerEnv: "1"},
	}
	c := NewClient(name, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClientLifecycle(t *testing.T) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}

	c := newTestServer(t, "fake")
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools/list got %+v", tools)
	}

	out, err := c.Call(context.Background(), "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != "echo:hello" {
		t.Fatalf("Call output = %q, want %q", out, "echo:hello")
	}
}

func TestBoundedMCPStderr(t *testing.T) {
	b := &boundedBuffer{limit: 16}
	input := strings.Repeat("x", 64)
	n, err := b.Write([]byte(input))
	if err != nil || n != len(input) {
		t.Fatalf("bounded stderr Write = (%d, %v), want all input consumed", n, err)
	}
	out := b.String()
	if !strings.HasPrefix(out, strings.Repeat("x", 16)) || !strings.Contains(out, "stderr truncated") {
		t.Fatalf("bounded stderr = %q", out)
	}
}

func TestMCPDiagnosticRedactsConfiguredSecretsAndLimitsTail(t *testing.T) {
	b := &boundedBuffer{limit: 16 * 1024}
	_, _ = b.Write([]byte(strings.Repeat("noise ", 2000) + "fatal token=s3cr3t Authorization: Bearer opaque https://user:pass@example.test/path"))
	diagnostic := mcpDiagnosticSuffix(b, ServerConfig{Env: map[string]string{"MCP_SECRET": "s3cr3t"}})
	if !strings.Contains(diagnostic, "fatal") || !strings.Contains(diagnostic, "stderr") {
		t.Fatalf("diagnostic omitted useful bounded failure text: %q", diagnostic)
	}
	for _, secret := range []string{"s3cr3t", "opaque", "user:pass"} {
		if strings.Contains(diagnostic, secret) {
			t.Fatalf("diagnostic leaked %q: %q", secret, diagnostic)
		}
	}
	if len(diagnostic) > 4*1024+32 { // suffix and marker overhead
		t.Fatalf("diagnostic exceeded bounded tail: %d", len(diagnostic))
	}
}

func TestResolveSSEEndpoint(t *testing.T) {
	tests := []struct {
		base, endpoint, want string
	}{
		{"http://example.test/sse", "/messages", "http://example.test/messages"},
		{"https://example.test/mcp/sse", "../messages", "https://example.test/messages"},
		{"http://example.test/mcp/", "messages", "http://example.test/mcp/messages"},
	}
	for _, tt := range tests {
		got, err := resolveSSEEndpoint(tt.base, tt.endpoint)
		if err != nil || got != tt.want {
			t.Errorf("resolveSSEEndpoint(%q, %q) = %q, %v; want %q", tt.base, tt.endpoint, got, err, tt.want)
		}
	}
	if _, err := resolveSSEEndpoint("file:///tmp/mcp", "/messages"); err == nil {
		t.Fatal("accepted non-http base URL")
	}
	if _, err := resolveSSEEndpoint("https://example.test/sse", "//attacker.test/messages"); err == nil {
		t.Fatal("accepted cross-origin protocol-relative SSE endpoint")
	}
}

func TestToolsListChangedStormUsesOneRefreshWorkerAndCloses(t *testing.T) {
	w := &refreshWriter{}
	c := NewClient("storm", ServerConfig{})
	c.stdin = nopWriteCloser{w}
	var changes atomic.Int32
	c.SetChangeListener(func() { changes.Add(1) })

	// Hold the first refresh at its response boundary, then send a large burst
	// of notifications. The coalescing worker should perform only one follow-up
	// refresh after the first response, rather than one goroutine per event.
	c.scheduleRefresh()
	id := waitPendingRequest(t, c)
	for i := 0; i < 1000; i++ {
		c.dispatchRaw([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
	}
	respondPending(t, c, id)
	id = waitPendingRequestAfter(t, c, id)
	respondPending(t, c, id)
	waitRefreshIdle(t, c)
	w.mu.Lock()
	calls := w.calls
	w.mu.Unlock()
	if calls != 2 || changes.Load() != 2 {
		t.Fatalf("notification storm caused %d refresh writes and %d callbacks, want 2/2", calls, changes.Load())
	}

	// A refresh blocked in requestJSON must also terminate when the client is
	// closed; this is the lifetime half of the storm bound.
	c2 := NewClient("storm-close", ServerConfig{})
	c2.stdin = nopWriteCloser{&refreshWriter{}}
	c2.scheduleRefresh()
	_ = waitPendingRequest(t, c2)
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}
	waitRefreshIdle(t, c2)
}

func waitPendingRequest(t *testing.T, c *Client) int {
	return waitPendingRequestAfter(t, c, 0)
}

func waitPendingRequestAfter(t *testing.T, c *Client, previous int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for id := range c.pending {
			if id != previous {
				c.mu.Unlock()
				return id
			}
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("refresh did not issue a pending tools/list request")
	return 0
}

func respondPending(t *testing.T, c *Client, id int) {
	t.Helper()
	c.dispatchRaw([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, id)))
}

func waitRefreshIdle(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.refreshMu.Lock()
		running := c.refreshRunning
		c.refreshMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("coalesced refresh worker remained running")
}

func TestManagerRegisterTools(t *testing.T) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}

	m := NewManager()
	m.Start(context.Background(), map[string]ServerConfig{
		"fake": {
			Command: os.Args[0],
			Args:    []string{"-test.run=TestManagerRegisterTools"},
			Env:     map[string]string{fakeServerEnv: "1"},
		},
	})
	defer m.Close()

	if len(m.Names()) != 1 || m.Names()[0] != "fake" {
		t.Fatalf("Names() = %v", m.Names())
	}

	reg := tools.NewRegistry()
	m.RegisterTools(reg)

	tool, ok := reg.Get("mcp__fake__echo")
	if !ok {
		t.Fatal("echo tool not registered")
	}
	if tool.Name() != "mcp__fake__echo" {
		t.Fatalf("tool name = %q, want qualified MCP name", tool.Name())
	}
	out, err := tool.Run(&tools.Context{
		Context: context.Background(),
		Args:    map[string]any{"text": "from-registry"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "echo:from-registry" {
		t.Fatalf("Run output = %q", out)
	}
}

func TestManagerStartCheckedRollsBackAllCandidates(t *testing.T) {
	m := NewManager()
	err := m.StartChecked(context.Background(), map[string]ServerConfig{
		"broken":          {Command: "/definitely/not/a/real/mcp-server"},
		"missing-command": {},
	})
	if err == nil {
		t.Fatal("StartChecked succeeded with an invalid candidate")
	}
	if got := m.Names(); len(got) != 0 {
		t.Fatalf("failed candidate partially published: %v", got)
	}
	m.Close()
}

func TestManagerRefreshKeepsOldStepBindingAlive(t *testing.T) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}
	server := ServerConfig{Command: os.Args[0], Args: []string{"-test.run=TestManagerRefreshKeepsOldStepBindingAlive"}, Env: map[string]string{fakeServerEnv: "1"}}
	m := NewManager()
	if err := m.StartChecked(context.Background(), map[string]ServerConfig{"fake": server}); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	reg := tools.NewRegistry()
	m.RegisterTools(reg)
	step := m.AcquireStep(reg)
	old, ok := step.Tools.Get("mcp__fake__echo")
	if !ok {
		t.Fatal("old step lost echo binding")
	}
	if err := m.Refresh(context.Background(), map[string]ServerConfig{"fake": server}); err != nil {
		t.Fatal(err)
	}
	current, ok := reg.Get("mcp__fake__echo")
	if !ok || current == old {
		t.Fatal("refresh did not publish a new binding")
	}
	if out, err := old.Run(&tools.Context{Context: context.Background(), Args: map[string]any{"text": "old"}}); err != nil || out != "echo:old" {
		t.Fatalf("old binding failed before step close: out=%q err=%v", out, err)
	}
	step.Close()
	if _, err := old.Run(&tools.Context{Context: context.Background(), Args: map[string]any{"text": "closed"}}); err == nil {
		t.Fatal("retired MCP client remained usable after step lease close")
	}
}

func TestManagerPrepareAbortDoesNotPublishCandidate(t *testing.T) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}
	server := ServerConfig{Command: os.Args[0], Args: []string{"-test.run=TestManagerPrepareAbortDoesNotPublishCandidate"}, Env: map[string]string{fakeServerEnv: "1"}}
	m := NewManager()
	if err := m.StartChecked(context.Background(), map[string]ServerConfig{"fake": server}); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	reg := tools.NewRegistry()
	m.RegisterTools(reg)
	old, ok := reg.Get("mcp__fake__echo")
	if !ok {
		t.Fatal("initial echo binding missing")
	}
	candidate, err := m.PrepareRefresh(context.Background(), map[string]ServerConfig{"fake": server}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if current, ok := reg.Get("mcp__fake__echo"); !ok || current != old {
		t.Fatalf("prepare published candidate: current=%T old=%T", current, old)
	}
	if err := candidate.Abort(); err != nil {
		t.Fatalf("abort candidate: %v", err)
	}
	if current, ok := reg.Get("mcp__fake__echo"); !ok || current != old {
		t.Fatalf("abort changed active binding: current=%T old=%T", current, old)
	}
}

func TestManagerStartsSSEWithoutCommand(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: endpoint\ndata: %s/messages\n\n", srv.URL)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		var req rpcMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		if req.Method == "tools/list" {
			result = map[string]any{"tools": []map[string]any{{
				"name": "sse_echo", "description": "echo", "inputSchema": map[string]any{"type": "object"},
			}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()

	m := NewManager()
	m.Start(context.Background(), map[string]ServerConfig{
		"remote": {Transport: "sse", BaseURL: srv.URL},
	})
	defer m.Close()
	if names := m.Names(); len(names) != 1 || names[0] != "remote" {
		t.Fatalf("SSE server without command was skipped: %v", names)
	}
	if got := m.ToolNames()["remote"]; len(got) != 1 || got[0] != "sse_echo" {
		t.Fatalf("SSE tools = %v", got)
	}
}

// nopWriteCloser adapts an io.Writer to io.WriteCloser for stdin fakes.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type refreshWriter struct {
	mu    sync.Mutex
	calls int
}

func (w *refreshWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	return len(p), nil
}

// TestFireClosedDispatchRace hammers dispatchRaw against fireClosed. dispatch
// pops a pending channel under the lock and sends after unlocking, so a
// fireClosed that closed those channels could race into "send on closed
// channel" and kill the process; entries are deleted instead now. Run with
// -race to verify.
func TestFireClosedDispatchRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := NewClient("fake", ServerConfig{})
		id := c.allocID()
		c.mu.Lock()
		c.pending[id] = make(chan json.RawMessage, 1)
		c.mu.Unlock()
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			c.dispatchRaw([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, id)))
		}()
		go func() {
			defer wg.Done()
			c.dispatchRaw([]byte(`{"jsonrpc":"2.0","id":99999,"result":{}}`))
		}()
		go func() {
			defer wg.Done()
			c.fireClosed()
		}()
		wg.Wait()
	}
}

// TestRequestJSONReturnsOnServerExit: blocked requestJSON callers must return
// promptly with a "server exited" error once fireClosed runs (previously they
// waited on a channel that was closed — now deleted — and must not leak), and
// a late response arriving after the delete must be dropped without panicking.
func TestRequestJSONReturnsOnServerExit(t *testing.T) {
	c := NewClient("fake", ServerConfig{})
	c.stdin = nopWriteCloser{io.Discard} // stdio writes go nowhere
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			var out json.RawMessage
			errs <- c.requestJSON(context.Background(), "tools/call", map[string]any{}, &out)
		}()
	}
	time.Sleep(50 * time.Millisecond) // let the requests register and block
	c.fireClosed()
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			if err == nil || !strings.Contains(err.Error(), "server exited") {
				t.Fatalf("requestJSON error = %v, want server exited", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("requestJSON still blocked after fireClosed")
		}
	}
	c.dispatchRaw([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) // late response: dropped
}

// TestCloseReapsSpontaneousExit: a server that exits on its own used to make
// Close an early-return no-op, leaving a zombie child behind. Close must still
// Kill+Wait so the child is reaped, and must not hang.
func TestCloseReapsSpontaneousExit(t *testing.T) {
	if os.Getenv(fakeServerEnv) == "1" {
		runFakeServer()
		return
	}
	c := newTestServer(t, "fake")
	if err := c.stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not observe the server exit")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !c.waited {
		t.Fatal("Close skipped cmd.Wait for an already-exited server (zombie)")
	}
	if c.cmd.ProcessState == nil {
		t.Fatal("cmd.ProcessState not set: child was not reaped")
	}
}

// TestDispatchRawEchoesServerRequestID: replies to server-initiated requests
// must echo the original ID verbatim — servers using string IDs are valid
// JSON-RPC and used to be silently dropped.
func TestDispatchRawEchoesServerRequestID(t *testing.T) {
	var buf bytes.Buffer
	c := NewClient("fake", ServerConfig{})
	c.stdin = nopWriteCloser{&buf}
	c.dispatchRaw([]byte(`{"jsonrpc":"2.0","id":"srv-7","method":"sampling/createMessage","params":{}}`))
	if buf.Len() == 0 {
		t.Fatal("no reply written for server request with string ID")
	}
	var msg rpcMessage
	if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
		t.Fatalf("reply %q: %v", buf.String(), err)
	}
	if string(msg.ID) != `"srv-7"` {
		t.Fatalf("reply id = %s, want \"srv-7\"", msg.ID)
	}
	if msg.Error == nil || msg.Error.Code != -32601 {
		t.Fatalf("reply error = %+v, want -32601", msg.Error)
	}
}

// TestDispatchRawStringIDResponseDropped: a response with a non-numeric ID can
// never match one of our int pending entries; it must be dropped silently.
func TestDispatchRawStringIDResponseDropped(t *testing.T) {
	var buf bytes.Buffer
	c := NewClient("fake", ServerConfig{})
	c.stdin = nopWriteCloser{&buf}
	c.dispatchRaw([]byte(`{"jsonrpc":"2.0","id":"str-id","result":{}}`))
	if buf.Len() != 0 {
		t.Fatalf("unexpected write: %q", buf.String())
	}
}
