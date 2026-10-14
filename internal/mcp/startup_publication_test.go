package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccdp/internal/tools"
)

func TestDeadCandidateNotPublished(t *testing.T) {
	m := NewManager()
	defer m.Close()
	reg := tools.NewRegistry()
	m.RegisterTools(reg)
	candidate, e := m.PrepareRefresh(context.Background(), nil, nil, false)
	if e != nil {
		t.Fatal(e)
	}
	c := NewClient("dead", ServerConfig{})
	c.tools = []ToolDef{{Name: "echo"}}
	candidate.clients = append(candidate.clients, refreshClient{name: "dead", client: c})
	c.fireClosed() // remote stream closes between successful prepare and publication
	if e = candidate.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, ok := reg.Get("mcp__dead__echo"); ok {
		t.Fatalf("dead server still published: manager names=%v", m.Names())
	}
}

func TestHTTPHeaderWaitHasStartupTimeout(t *testing.T) {
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done() // connected peer never writes response headers
	}))
	defer srv.Close()
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- m.StartChecked(ctx, map[string]ServerConfig{"remote": {Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true}})
	}()
	<-entered
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("startup unexpectedly succeeded")
		}
	case <-time.After(startupTimeout + 250*time.Millisecond):
		cancel()
		e := <-done
		t.Fatalf("HTTP headers exceeded declared %s startup bound; only external cancellation settled it: %v", startupTimeout, e)
	}
}

func TestSSEDisconnectBeforeCommitUnregisters(t *testing.T) {
	disconnect := make(chan struct{})
	srv := newLegacyTestServer(t, disconnect, "echo")
	defer srv.Close()
	m := NewManager()
	defer m.Close()
	reg := tools.NewRegistry()
	m.RegisterTools(reg)
	candidate, e := m.PrepareRefresh(context.Background(), map[string]ServerConfig{"remote": {Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true}}, nil, false)
	if e != nil {
		t.Fatal(e)
	}
	c := candidate.clients[0].client
	close(disconnect)
	select {
	case <-c.closed:
	case <-time.After(time.Second):
		candidate.Abort()
		t.Fatal("did not observe disconnected SSE")
	}
	if e = candidate.Commit(); e != nil {
		t.Fatal(e)
	}
	if _, ok := reg.Get("mcp__remote__echo"); ok {
		t.Fatalf("disconnected SSE client exposed to next model step: manager names=%v", m.Names())
	}
}

func TestLateCloseListenerRunsOnceAndCanReenterClose(t *testing.T) {
	c := NewClient("late", ServerConfig{})
	c.fireClosed()
	done := make(chan struct{})
	calls := 0
	go func() {
		c.SetCloseListener(func() { calls++; _ = c.Close() })
		c.fireClosed()
		c.SetCloseListener(func() { calls++ })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close notification deadlocked on reentry")
	}
	if calls != 1 {
		t.Fatalf("close notifications=%d want1", calls)
	}
}
