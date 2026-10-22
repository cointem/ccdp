package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"ccdp/internal/tools"
)

func TestManagerRejectsCollidingCatalogBeforePublication(t *testing.T) {
	cases := []struct{ serverA, toolA, serverB, toolB string }{
		{"org", "repo__read", "org__repo", "read"},
		{"a_", "b", "a", "_b"},
	}
	for _, tc := range cases {
		t.Run(tc.serverA+"/"+tc.serverB, func(t *testing.T) {
			a := newLegacyTestServer(t, nil, tc.toolA)
			defer a.Close()
			b := newLegacyTestServer(t, nil, tc.toolB)
			defer b.Close()
			servers := map[string]ServerConfig{
				tc.serverA: {Transport: "sse", BaseURL: a.URL, NetworkAuthorized: true},
				tc.serverB: {Transport: "sse", BaseURL: b.URL, NetworkAuthorized: true},
			}
			m := NewManager()
			defer m.Close()
			reg := tools.NewRegistry()
			if err := m.RegisterTools(reg); err != nil {
				t.Fatal(err)
			}
			err := m.StartChecked(context.Background(), servers)
			assertCollision(t, err, tc.serverA, tc.toolA, tc.serverB, tc.toolB)
			if len(m.Names()) != 0 || len(reg.Names()) != 0 {
				t.Fatal("failed startup published part of the ambiguous catalog")
			}
		})
	}
}

func TestPrepareRefreshCollisionPreservesActiveCatalog(t *testing.T) {
	a := newLegacyTestServer(t, nil, "repo__read")
	defer a.Close()
	b := newLegacyTestServer(t, nil, "read")
	defer b.Close()
	m := NewManager()
	defer m.Close()
	old := NewClient("existing", ServerConfig{})
	old.tools = []ToolDef{{Name: "safe"}}
	m.clients["existing"] = old
	m.names = []string{"existing"}
	reg := tools.NewRegistry()
	if err := m.RegisterTools(reg); err != nil {
		t.Fatal(err)
	}
	oldBinding, _ := reg.Get("mcp__existing__safe")
	candidate, err := m.PrepareRefresh(context.Background(), map[string]ServerConfig{
		"org":       {Transport: "sse", BaseURL: a.URL, NetworkAuthorized: true},
		"org__repo": {Transport: "sse", BaseURL: b.URL, NetworkAuthorized: true},
	}, nil, false)
	if candidate != nil {
		_ = candidate.Abort()
		t.Fatal("ambiguous catalog passed preparation")
	}
	assertCollision(t, err, "org", "repo__read", "org__repo", "read")
	binding, ok := reg.Get("mcp__existing__safe")
	client, _ := m.Server("existing")
	if !ok || binding != oldBinding || client != old || len(m.Names()) != 1 {
		t.Fatal("failed candidate changed the active generation")
	}
}

func TestRegisterToolsRejectsDuplicateDefinitions(t *testing.T) {
	m := NewManager()
	defer m.Close()
	c := NewClient("same", ServerConfig{})
	c.tools = []ToolDef{{Name: "read"}, {Name: "read"}}
	m.clients["same"] = c
	m.names = []string{"same"}
	reg := tools.NewRegistry()
	if err := m.RegisterTools(reg); err == nil {
		t.Fatal("duplicate definitions were silently overwritten")
	}
	if len(reg.Names()) != 0 {
		t.Fatal("registration partially published a rejected catalog")
	}
}

func TestListChangedCollisionKeepsAcceptedListAndRegistry(t *testing.T) {
	client, peer := newWireClient(t)
	client.mu.Lock()
	client.tools = []ToolDef{{Name: "safe"}}
	client.mu.Unlock()
	other := NewClient("wire__repo", ServerConfig{})
	other.tools = []ToolDef{{Name: "read"}}
	m := NewManager()
	defer m.Close()
	m.clients = map[string]*Client{"wire": client, "wire__repo": other}
	m.names = []string{"wire", "wire__repo"}
	reg := tools.NewRegistry()
	if err := m.RegisterTools(reg); err != nil {
		t.Fatal(err)
	}
	oldBinding, _ := reg.Get("mcp__wire__safe")
	for _, rawName := range []string{"repo__read", "new-safe"} {
		if err := peer.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"}); err != nil {
			t.Fatal(err)
		}
		request := peer.next(t)
		if request.Method != "tools/list" {
			t.Fatalf("request method=%q", request.Method)
		}
		if err := peer.reply(request, testTools(rawName)); err != nil {
			t.Fatal(err)
		}
		waitRefreshIdle(t, client)
		defs := client.Tools()
		if rawName == "repo__read" {
			binding, ok := reg.Get("mcp__wire__safe")
			if len(defs) != 1 || defs[0].Name != "safe" || !ok || binding != oldBinding {
				t.Fatal("rejected list replaced accepted client tools or registry binding")
			}
			collision, ok := reg.Get("mcp__wire__repo__read")
			if !ok || collision.(*mcpTool).client != other {
				t.Fatal("rejected list replaced the other server's tool")
			}
		} else {
			binding, ok := reg.Get("mcp__wire__new-safe")
			if len(defs) != 1 || defs[0].Name != "new-safe" || !ok || binding.(*mcpTool).client != client {
				t.Fatal("valid list after rejection did not update the catalog")
			}
			if _, ok := reg.Get("mcp__wire__safe"); ok {
				t.Fatal("valid list left an obsolete tool registered")
			}
		}
	}
}

func TestPreparedClientDefersListChangesUntilPublished(t *testing.T) {
	client, peer := newWireClient(t)
	client.pauseToolRefresh()
	replied := make(chan error, 1)
	go func() {
		request := peer.next(t)
		replied <- peer.reply(request, testTools("after-publish"))
	}()
	// The same coalesced refresh entry point is used by SDK notifications.
	client.scheduleRefresh()
	client.scheduleRefresh()
	waitRefreshIdle(t, client)
	if len(client.Tools()) != 0 {
		t.Fatal("prepared client's accepted catalog changed")
	}
	m := NewManager()
	defer m.Close()
	m.clients["wire"] = client
	m.names = []string{"wire"}
	reg := tools.NewRegistry()
	if err := m.RegisterTools(reg); err != nil {
		t.Fatal(err)
	}
	client.resumeToolRefresh()
	select {
	case err := <-replied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deferred list refresh did not reach the server after publication")
	}
	waitRefreshIdle(t, client)
	if _, ok := reg.Get("mcp__wire__after-publish"); !ok {
		t.Fatal("notification received during preparation was lost")
	}
}

func assertCollision(t *testing.T, err error, names ...string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("expected namespace collision, got %v", err)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), `"`+name+`"`) {
			t.Fatalf("collision diagnostic %q omits origin %q", err, name)
		}
	}
}
