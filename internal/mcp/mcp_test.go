package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
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

	tool, ok := reg.Get("echo")
	if !ok {
		t.Fatal("echo tool not registered")
	}
	if tool.Name() != "echo" {
		t.Fatalf("tool name = %q", tool.Name())
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

// nopWriteCloser adapts an io.Writer to io.WriteCloser for stdin fakes.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

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
