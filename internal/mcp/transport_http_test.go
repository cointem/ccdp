package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func initializeResult() any {
	return map[string]any{"protocolVersion": httpProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "test", "version": "1"}}
}
func testTools(name string) any {
	return map[string]any{"tools": []map[string]any{{"name": name, "inputSchema": map[string]any{"type": "object"}}}}
}

func newHTTPTestClient(t *testing.T, call func(http.ResponseWriter, *http.Request, rpcMessage)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req rpcMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "session-123")
			writeRPC(w, req.ID, initializeResult())
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "session-123" || r.Header.Get("Mcp-Protocol-Version") != httpProtocolVersion {
			t.Errorf("missing session/protocol headers: %v", r.Header)
			w.WriteHeader(400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		if req.Method == "tools/list" {
			writeRPC(w, req.ID, testTools("echo"))
			return
		}
		call(w, r, req)
	}))
	t.Cleanup(srv.Close)
	c := NewClient("http", ServerConfig{Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true})
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return c
}

// A real legacy peer delivers replies on GET, with POST only acknowledging.
func newLegacyTestServer(t *testing.T, disconnect <-chan struct{}, tool string) *httptest.Server {
	t.Helper()
	replies := make(chan []byte, 16)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: endpoint\ndata: /messages\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case reply := <-replies:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", reply)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				case <-disconnect:
					return
				}
			}
		}
		if r.URL.Path != "/messages" {
			w.WriteHeader(405)
			return
		}
		var req rpcMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(req.ID) > 0 {
			var result any = map[string]any{}
			switch req.Method {
			case "initialize":
				legacy := initializeResult().(map[string]any)
				legacy["protocolVersion"] = ProtocolVersion
				result = legacy
			case "tools/list":
				result = testTools(tool)
			}
			reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			select {
			case replies <- reply:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(202)
	}))
}

func TestHTTPPostOnlyServerAndStartupLifetime(t *testing.T) {
	c := newHTTPTestClient(t, func(w http.ResponseWriter, r *http.Request, req rpcMessage) {
		writeRPC(w, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}})
	})
	// newHTTPTestClient has cancelled the successful startup operation already.
	out, err := c.Call(context.Background(), "echo", nil)
	if err != nil || out != "ok" {
		t.Fatalf("call=%q err=%v", out, err)
	}
}

func TestHTTPResponseStreamEOFSettlesOnlyItsRequest(t *testing.T) {
	var calls atomic.Int32
	slowEntered, releaseSlow := make(chan struct{}), make(chan struct{})
	c := newHTTPTestClient(t, func(w http.ResponseWriter, r *http.Request, req rpcMessage) {
		calls.Add(1)
		var params struct {
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if params.Arguments["slow"] == true {
			close(slowEntered)
			select {
			case <-releaseSlow:
			case <-r.Context().Done():
				return
			}
			writeRPC(w, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":\"t\",\"progress\":1}}\n\n")
	})
	slow := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "echo", map[string]any{"slow": true}); slow <- err }()
	<-slowEntered
	failed := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "echo", nil); failed <- err }()
	err := receiveError(t, failed)
	close(releaseSlow)
	if err == nil || !strings.Contains(err.Error(), "without response") {
		t.Fatalf("EOF error=%v", err)
	}
	if err := receiveError(t, slow); err != nil {
		t.Fatalf("unrelated call failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected automatic POST replay: %d", calls.Load())
	}
}

func TestHTTPResponseStreamLivesUntilRequestSettles(t *testing.T) {
	ended := make(chan struct{})
	c := newHTTPTestClient(t, func(w http.ResponseWriter, r *http.Request, req rpcMessage) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Multiline data exercises actual SSE event framing.
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\n"+"data: \"id\":%s,\"result\":{\"content\":[]}}\n\n", req.ID)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(ended)
	})
	if _, err := c.Call(context.Background(), "echo", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("response body not released")
	}
}

func TestHTTPPostUsesRequestAndConnectionCancellation(t *testing.T) {
	for _, connection := range []bool{false, true} {
		t.Run(fmt.Sprint(connection), func(t *testing.T) {
			entered, ended := make(chan struct{}), make(chan struct{})
			c := newHTTPTestClient(t, func(w http.ResponseWriter, r *http.Request, req rpcMessage) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(entered)
				<-r.Context().Done()
				close(ended)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.Call(ctx, "echo", nil); done <- err }()
			<-entered
			if connection {
				_ = c.Close()
			} else {
				cancel()
			}
			if err := receiveError(t, done); err == nil || (!connection && !errors.Is(err, context.Canceled)) {
				t.Fatalf("cancel error=%v", err)
			}
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("request body not closed")
			}
		})
	}
}

func TestHTTPDoesNotFallbackOnAuthorizationFailure(t *testing.T) {
	for _, status := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					gets.Add(1)
				}
				w.WriteHeader(status)
			}))
			defer srv.Close()
			c := NewClient("denied", ServerConfig{Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true})
			defer c.Close()
			if err := c.Start(context.Background()); err == nil {
				t.Fatal("expected HTTP error")
			}
			if gets.Load() != 0 {
				t.Fatalf("fallback on status %d", status)
			}
		})
	}
}

func TestHTTPRejectsCrossOriginLegacyEndpoint(t *testing.T) {
	var leaked atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); w.WriteHeader(202) }))
	defer outside.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: endpoint\ndata: %s/messages\n\n", outside.URL)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewClient("origin", ServerConfig{Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Start(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected immediate origin error: %v", err)
	}
	if leaked.Load() != 0 {
		t.Fatal("request reached unapproved origin")
	}
}

func TestHTTPInteroperatesWithOfficialServer(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "official", Version: "1"}, nil)
	handler := func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "official response"}}}, nil
	}
	server.AddTool(&sdk.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, handler)
	stream := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	connected := make(chan struct{}, 1)
	var terminated atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
		if r.Method == http.MethodDelete {
			terminated.Store(true)
		}
		stream.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := NewClient("official", ServerConfig{Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if out, err := c.Call(ctx, "echo", nil); err != nil || out != "official response" {
		t.Fatalf("call=%q err=%v", out, err)
	}
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("notification stream did not open")
	}
	changed := make(chan struct{}, 1)
	c.SetChangeListener(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	server.AddTool(&sdk.Tool{Name: "new_tool", InputSchema: map[string]any{"type": "object"}}, handler)
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("tool change was not delivered")
	}
	if len(c.Tools()) != 2 {
		t.Fatalf("tools=%v", c.Tools())
	}
	_ = c.Close()
	if !terminated.Load() {
		t.Fatal("session termination DELETE was not sent")
	}
}

func TestHTTPResponseStreamReadErrorSettles(t *testing.T) {
	c := newHTTPTestClient(t, func(w http.ResponseWriter, r *http.Request, req rpcMessage) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "512")
		fmt.Fprint(w, ": keepalive\n\n") // truncated HTTP body produces a scanner read error
	})
	done := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "echo", nil); done <- err }()
	if err := receiveError(t, done); err == nil {
		t.Fatal("truncated stream unexpectedly succeeded")
	}
}
