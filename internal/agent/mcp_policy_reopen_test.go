package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/mcp"
	"ccdp/internal/protocol"
)

func TestExtraReviewMCPReloadAfterSandboxReset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		if req.Method == "initialize" {
			result = map[string]any{"protocolVersion": req.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "test", "version": "1"}}
		}
		if req.Method == "tools/list" {
			result = map[string]any{"tools": []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()
	cfg := testPersistenceConfig(t)
	cfg.NetworkAccess = true
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.MCPServers = map[string]mcp.ServerConfig{"remote": {Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true}}
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: frozenProviderRegistry(cfg.Model)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(a.MCPServers()) != 1 {
		t.Fatal("fixture MCP did not start")
	}
	for i, allow := range []bool{false, true} {
		cmd := protocol.Command{ID: protocol.CommandID(fmt.Sprintf("sandbox-%d", i)), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandSetSandboxPolicy, SandboxPolicy: &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{NetworkAccess: allow}}}
		r := a.applySandboxPolicy(cmd)
		if r.Rejected() {
			t.Fatal(r.Error)
		}
	}
	if err = a.ReloadSettings(); err != nil {
		t.Fatal(err)
	}
	if len(a.MCPServers()) != 1 {
		t.Fatal("reload leaves configured MCP permanently disconnected after policy reset")
	}
	_, _ = a.Snapshot(context.Background())
}
