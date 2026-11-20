package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"ccdp/internal/sandbox"
	"ccdp/internal/tools"
)

type exaTestTransport func(*http.Request) (*http.Response, error)

func (f exaTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExaMCPWire(t *testing.T) {
	const key = "test-key"
	for _, tc := range []struct {
		name            string
		status          int
		body            string
		limited         bool
		interruptMethod string
		interruptError  error
	}{
		{name: "search", status: 200},
		{name: "tool quota metadata", status: 200, body: `{"_meta":{"ai.exa/rateLimited":true},"content":[]}`, limited: true},
		{name: "anonymous quota notice", status: 200, body: `{"content":[{"type":"text","text":"You've hit Exa's free MCP rate limit. More info"}]}`, limited: true},
		{name: "tool credit limit", status: 200, body: `{"isError":true,"content":[{"type":"text","text":"web_search_advanced_exa error (402): account budget exceeded"}]}`, limited: true},
		{name: "rate limit", status: 429, limited: true},
		{name: "credits exhausted", status: 402, body: `{"tag":"NO_MORE_CREDITS"}`, limited: true},
		{name: "payment verification", status: 402, body: `{"tag":"X402_PAYMENT_REQUIRED","error":"provide a key to use account credits"}`},
		{name: "authentication", status: 401},
		{name: "redirect", status: 307},
		{name: "reconnect after initialization EOF", status: 200, interruptMethod: "initialize", interruptError: io.EOF},
		{name: "reconnect after initialization timeout", status: 200, interruptMethod: "initialize", interruptError: os.ErrDeadlineExceeded},
		{name: "no replay after search EOF", status: 200, interruptMethod: "tools/call", interruptError: io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, initializations int
			mock := exaTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != exaEndpoint || req.Method != http.MethodPost || req.Header.Get("x-api-key") != key {
					t.Errorf("unexpected MCP destination, method or credentials")
				}
				var msg struct {
					ID     json.RawMessage
					Method string
					Params struct {
						Name      string
						Arguments map[string]any
					}
				}
				if err := json.NewDecoder(req.Body).Decode(&msg); err != nil {
					return nil, err
				}
				status := http.StatusOK
				var result, body string
				switch msg.Method {
				case "initialize":
					initializations++
					if tc.interruptMethod == msg.Method && initializations == 1 {
						return nil, tc.interruptError
					}
					result = `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"exa","version":"1"}}`
				case "notifications/initialized":
					status = http.StatusAccepted
				case "tools/list":
					result = `{"tools":[{"name":"web_search_advanced_exa","inputSchema":{"type":"object"}}]}`
				case "tools/call":
					calls++
					if tc.interruptMethod == msg.Method {
						return nil, tc.interruptError
					}
					args := msg.Params.Arguments
					if msg.Params.Name != exaSearchTool || len(args) != 3 || args["query"] != "example" || args["numResults"] != float64(1) || args["enableHighlights"] != true {
						t.Errorf("unexpected tool parameters: %+v", msg.Params)
					}
					status = tc.status
					if status == 200 {
						result = `{"structuredContent":{"results":[{"url":"https://example.com","title":"Example","highlights":["a","b"]}]},"content":[]}`
						if tc.body != "" {
							result = tc.body
						}
					} else {
						body = tc.body
					}
				default:
					return nil, fmt.Errorf("unexpected RPC method %s", msg.Method)
				}
				if result != "" {
					body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, msg.ID, result)
				}
				h := http.Header{"Content-Type": {"application/json"}}
				if status == 307 {
					h.Set("Location", "https://other.example")
				}
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			cfg := DefaultExaServer()
			cfg.HTTPHeaders = map[string]string{"x-api-key": key}
			client := NewClient(ExaServer, cfg)
			defer client.Close()
			if err := client.connectHTTP(t.Context(), mock); err != nil {
				t.Fatal(err)
			}
			tool := &mcpTool{client: client, def: client.Tools()[0]}
			response, err := tool.Execute(&tools.Context{Context: t.Context(), HostNetworkAllowed: true}, map[string]any{"query": "example", "numResults": 1, "enableHighlights": true})
			wantInit := 1
			if tc.interruptMethod == "initialize" {
				wantInit = 2
			}
			if initializations != wantInit || calls != 1 || errors.Is(err, errExaQuota) != tc.limited || (err == nil) != (tc.status == 200 && !tc.limited && tc.interruptMethod != "tools/call") {
				t.Fatalf("initializations=%d, calls=%d, reply=%v, err=%v", initializations, calls, response, err)
			}
			if err == nil && !strings.Contains(string(response.Data), "https://example.com") {
				t.Fatal("search lost structured content")
			}
		})
	}
}

func TestOnDemandMCPRegistry(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Host != "example.com" {
			t.Errorf("unexpected destination %q", r.Host)
		}
		var call rpcMessage
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		switch call.Method {
		case "initialize":
			writeRPC(w, call.ID, initializeResult())
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPC(w, call.ID, testTools("lookup"))
		case "tools/call":
			writeRPC(w, call.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "registered response"}}})
		default:
			t.Errorf("unexpected RPC method %q", call.Method)
		}
	}))
	defer proxy.Close()
	manager := NewManager()
	defer manager.Close()
	sb := sandbox.New(t.TempDir())
	sb.NetworkProxy = proxy.URL
	manager.SetSandbox(sb)
	cfg := ServerConfig{Transport: "http", BaseURL: "http://example.com/mcp", NetworkAuthorized: true, OnDemandTools: []ToolDef{{Name: "lookup"}}}
	if err := manager.StartChecked(t.Context(), map[string]ServerConfig{"catalog": cfg}); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	if err := manager.RegisterTools(reg); err != nil {
		t.Fatal(err)
	}
	tool, ok := reg.Get("mcp__catalog__lookup")
	if !ok || requests.Load() != 0 {
		t.Fatal("offline discovery contacted the server or omitted its tool")
	}
	ctx := &tools.Context{Context: t.Context()}
	if _, err := tool.Execute(ctx, nil); err == nil || requests.Load() != 0 {
		t.Fatal("unauthorized call reached server")
	}
	ctx.HostNetworkAllowed = true
	result, err := tool.Execute(ctx, nil)
	if err != nil || result.Text != "registered response" || requests.Load() != 4 {
		t.Fatalf("result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
}

func TestExaInvocationCredentialScope(t *testing.T) {
	cfg := DefaultExaServer()
	cfg.HTTPHeaders = map[string]string{"x-api-key": "old-key"}
	client := NewClient(ExaServer, cfg)
	tool := &mcpTool{client: client, def: cfg.OnDemandTools[0]}
	ctx := BindExaCredential(t.Context(), tool, "new-key")
	if client.requestHeaders(ctx)["x-api-key"] != "new-key" || client.cfg.HTTPHeaders["x-api-key"] != "old-key" || client.redact(ctx, "invalid new-key") != "invalid [redacted]" {
		t.Fatal("rotated credentials did not stay scoped to this invocation")
	}
	if client.requestHeaders(BindExaCredential(t.Context(), tool, ""))["x-api-key"] != "" {
		t.Fatal("clearing the key left the old credential active")
	}
	cfg.HTTPHeaders = nil
	cfg.BaseURL = "https://example.com/mcp"
	other := &mcpTool{client: NewClient(ExaServer, cfg), def: cfg.OnDemandTools[0]}
	if IsExaSearch(other) || len(other.client.requestHeaders(ctx)) != 0 || len(other.client.requestHeaders(BindExaCredential(t.Context(), other, "new-key"))) != 0 {
		t.Fatal("a replacement endpoint inherited the hosted service identity or its credential")
	}
}
