package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"ccdp/internal/sandbox"
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
	c.SetSandbox(sandbox.New(t.TempDir()))
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
	m.SetSandbox(sandbox.New(t.TempDir()))
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
	m.SetSandbox(sandbox.New(t.TempDir()))
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
	m.SetSandbox(sandbox.New(t.TempDir()))
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
	m.SetSandbox(sandbox.New(t.TempDir()))
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
	srv := newLegacyTestServer(t, nil, "sse_echo")
	defer srv.Close()

	// The in-process SSE transport connects only to this loopback test server.
	sb := sandbox.New(t.TempDir())
	sb.SetAllowNetwork(true)
	m := NewManager()
	m.SetSandbox(sb)
	m.Start(context.Background(), map[string]ServerConfig{
		"remote": {Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true},
	})
	defer m.Close()
	if names := m.Names(); len(names) != 1 || names[0] != "remote" {
		t.Fatalf("SSE server without command was skipped: %v", names)
	}
	if got := m.ToolNames()["remote"]; len(got) != 1 || got[0] != "sse_echo" {
		t.Fatalf("SSE tools = %v", got)
	}
}

func TestUnapprovedRemoteMCPDoesNotConnect(t *testing.T) {
	client := NewClient("remote", ServerConfig{Transport: "sse", BaseURL: "http://127.0.0.1:1/sse"})
	err := client.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "explicit network_authorized consent") {
		t.Fatalf("unapproved remote MCP start = %v", err)
	}
}

func TestRemoteMCPToolRequiresPerCallHostNetwork(t *testing.T) {
	client := NewClient("remote", ServerConfig{Transport: "sse", BaseURL: "https://example.test/sse", NetworkAuthorized: true})
	tool := &mcpTool{client: client, def: ToolDef{Name: "read"}}
	if _, err := tool.Run(&tools.Context{Context: context.Background(), Args: map[string]any{}}); err == nil || !strings.Contains(err.Error(), "per-call outbound network authorization") {
		t.Fatalf("remote MCP call without per-call authorization = %v", err)
	}
}

func TestSSERedirectMustStayOnConfiguredOrigin(t *testing.T) {
	base, _ := url.Parse("https://mcp.example.test:8443/sse")
	for _, raw := range []string{"https://mcp.example.test:8443/next", "https://other.example.test:8443/next", "https://mcp.example.test/next"} {
		target, _ := url.Parse(raw)
		err := sameSSEOriginRedirect(&http.Request{URL: target}, []*http.Request{{URL: base}})
		if sameHTTPOrigin(base, target) && err != nil {
			t.Errorf("same-origin redirect %s rejected: %v", raw, err)
		}
		if !sameHTTPOrigin(base, target) && err == nil {
			t.Errorf("cross-origin redirect %s allowed", raw)
		}
	}
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
	select {
	case <-c.cmd.Done():
	default:
		t.Fatal("child was not reaped")
	}
}
