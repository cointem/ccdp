package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
