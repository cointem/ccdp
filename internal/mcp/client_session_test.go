package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type wirePeer struct {
	conn     net.Conn
	mu       sync.Mutex
	messages chan rpcMessage
}

func (p *wirePeer) send(v any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return json.NewEncoder(p.conn).Encode(v)
}
func (p *wirePeer) next(t *testing.T) rpcMessage {
	t.Helper()
	for {
		select {
		case m := <-p.messages:
			if m.Method == "notifications/initialized" || m.Method == "notifications/cancelled" {
				continue
			}
			return m
		case <-time.After(2 * time.Second):
			t.Fatal("peer did not receive a message")
			return rpcMessage{}
		}
	}
}
func (p *wirePeer) reply(m rpcMessage, result any) error {
	return p.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
}
func newWireClient(t *testing.T) (*Client, *wirePeer) {
	t.Helper()
	local, remote := net.Pipe()
	peer := &wirePeer{conn: remote, messages: make(chan rpcMessage, 64)}
	go func() {
		defer remote.Close()
		scanner := bufio.NewScanner(remote)
		scanner.Buffer(make([]byte, 4096), maxLineLen)
		initialList := true
		for scanner.Scan() {
			var m rpcMessage
			if json.Unmarshal(scanner.Bytes(), &m) != nil {
				return
			}
			switch {
			case m.Method == "initialize":
				_ = peer.reply(m, map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "peer", "version": "1"}})
			case m.Method == "tools/list" && initialList:
				initialList = false
				_ = peer.reply(m, map[string]any{"tools": []any{}})
			default:
				peer.messages <- m
			}
		}
	}()
	c := NewClient("wire", ServerConfig{})
	t.Cleanup(func() { _ = remote.Close(); _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.connectStdio(ctx, local, local); err != nil {
		t.Fatal(err)
	}
	return c, peer
}
func TestToolsListChangedStormUsesOneRefreshWorkerAndCloses(t *testing.T) {
	c, peer := newWireClient(t)
	var changes atomic.Int32
	c.SetChangeListener(func() { changes.Add(1) })
	c.scheduleRefresh()
	first := peer.next(t)
	for i := 0; i < 1000; i++ {
		c.scheduleRefresh()
	}
	if err := peer.reply(first, map[string]any{"tools": []any{}}); err != nil {
		t.Fatal(err)
	}
	second := peer.next(t)
	if first.Method != "tools/list" || second.Method != "tools/list" || string(first.ID) == string(second.ID) {
		t.Fatal("invalid refresh requests")
	}
	if err := peer.reply(second, map[string]any{"tools": []any{}}); err != nil {
		t.Fatal(err)
	}
	waitRefreshIdle(t, c)
	if changes.Load() != 2 {
		t.Fatalf("callbacks=%d, want 2", changes.Load())
	}
	c.scheduleRefresh()
	_ = peer.next(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	waitRefreshIdle(t, c)
}
func TestSDKStdioNotificationsRefreshTools(t *testing.T) {
	c, peer := newWireClient(t)
	changed := make(chan struct{}, 1)
	c.SetChangeListener(func() { changed <- struct{}{} })
	if err := peer.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"}); err != nil {
		t.Fatal(err)
	}
	request := peer.next(t)
	if request.Method != "tools/list" {
		t.Fatalf("request=%s", request.Method)
	}
	_ = peer.reply(request, map[string]any{"tools": []any{map[string]any{"name": "new", "inputSchema": map[string]any{"type": "object"}}}})
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh callback missing")
	}
	if defs := c.Tools(); len(defs) != 1 || defs[0].Name != "new" {
		t.Fatalf("tools=%+v", defs)
	}
}
func TestSDKPendingCallsSettleOnServerExit(t *testing.T) {
	c, peer := newWireClient(t)
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() { _, err := c.Call(context.Background(), "echo", nil); errs <- err }()
	}
	for i := 0; i < n; i++ {
		_ = peer.next(t)
	}
	_ = peer.conn.Close()
	for i := 0; i < n; i++ {
		if err := receiveError(t, errs); err == nil {
			t.Fatal("server exit returned success")
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestSDKStdioEchoesServerRequestID(t *testing.T) {
	_, peer := newWireClient(t)
	if err := peer.send(map[string]any{"jsonrpc": "2.0", "id": "srv-7", "method": "sampling/createMessage", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	reply := peer.next(t)
	if string(reply.ID) != `"srv-7"` || reply.Error == nil {
		t.Fatalf("reply=%+v", reply)
	}
}
func TestSDKStdioUnmatchedResponsePreservesSession(t *testing.T) {
	c, peer := newWireClient(t)
	if err := peer.send(map[string]any{"jsonrpc": "2.0", "id": "unknown", "result": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "echo", nil); done <- err }()
	req := peer.next(t)
	_ = peer.reply(req, map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}})
	if err := receiveError(t, done); err != nil {
		t.Fatal(err)
	}
}
