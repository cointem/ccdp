package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"ccdp/internal/messages"
)

func catalogTool(name, description string) ToolDef {
	return ToolDef{Type: "function", Function: FuncDef{Name: name, Description: description, Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}}
}

func catalogRequest(current []ToolDef, update *messages.ToolUpdate) CompletionRequest {
	base := catalogTool("base", "original")
	return CompletionRequest{Model: "selected/model", Tools: current, Messages: []ChatMessage{
		{Role: "system", Content: "base instructions"},
		{Role: "system", ToolUpdate: &messages.ToolUpdate{Initial: true, Added: toolDefinitions([]ToolDef{base})}},
		{Role: "user", Content: "before"},
		{Role: "system", ToolUpdate: update},
	}}
}

func decodeCatalogBody(t *testing.T, wire string, req CompletionRequest) map[string]any {
	t.Helper()
	cfg := Config{Wire: wire, APIModel: "api-model", BaseURL: "https://gateway.example/v1"}
	return decodeCatalogProviderBody(t, cfg, req)
}

func decodeCatalogProviderBody(t *testing.T, cfg Config, req CompletionRequest) map[string]any {
	t.Helper()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.marshalRequestBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "tool_update") {
		t.Fatalf("internal metadata on wire: %s", body)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestResponsesIncrementalToolsAdditionAndFallback(t *testing.T) {
	base, added := catalogTool("base", "original"), catalogTool("late", "new")
	initialReq := catalogRequest([]ToolDef{base}, nil)
	initialReq.Messages = initialReq.Messages[:3]
	initial := decodeCatalogBody(t, "responses", initialReq)
	addition := catalogRequest([]ToolDef{base, added}, &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{added})})
	doc := decodeCatalogBody(t, "responses", addition)
	if !reflect.DeepEqual(initial["tools"], doc["tools"]) {
		t.Fatal("addition rewrote top-level tools")
	}
	before, after := initial["input"].([]any), doc["input"].([]any)
	if len(after) != len(before)+1 || !reflect.DeepEqual(before, after[:len(before)]) {
		t.Fatalf("prefix changed: %v", after)
	}
	update := after[len(before)].(map[string]any)
	if update["type"] != "additional_tools" || update["role"] != "developer" || len(update["tools"].([]any)) != 1 {
		t.Fatalf("bad Responses update: %v", update)
	}
	for _, tc := range []struct {
		name string
		req  CompletionRequest
	}{
		{"removed", catalogRequest([]ToolDef{added}, &messages.ToolUpdate{Removed: []string{"base"}, Added: toolDefinitions([]ToolDef{added})})},
		{"changed", catalogRequest([]ToolDef{catalogTool("base", "changed")}, &messages.ToolUpdate{Removed: []string{"base"}, Added: toolDefinitions([]ToolDef{catalogTool("base", "changed")})})},
		{"mismatch", catalogRequest([]ToolDef{base}, &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{added})})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallback := decodeCatalogBody(t, "responses", tc.req)
			if len(fallback["input"].([]any)) != len(before) {
				t.Fatalf("control message leaked: %v", fallback)
			}
			if len(fallback["tools"].([]any)) != len(tc.req.Tools) {
				t.Fatalf("wrong current tools: %v", fallback)
			}
		})
	}
}

func TestChatToolUpdatesUseCurrentCatalog(t *testing.T) {
	base, added, changed := catalogTool("base", "original"), catalogTool("late", "new"), catalogTool("base", "changed")
	for _, tc := range []struct {
		name    string
		current []ToolDef
		update  *messages.ToolUpdate
	}{
		{"add", []ToolDef{base, added}, &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{added})}},
		{"remove all", nil, &messages.ToolUpdate{Removed: []string{"base"}}},
		{"replace", []ToolDef{added}, &messages.ToolUpdate{Removed: []string{"base"}, Added: toolDefinitions([]ToolDef{added})}},
		{"redefine", []ToolDef{changed}, &messages.ToolUpdate{Removed: []string{"base"}, Added: toolDefinitions([]ToolDef{changed})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := catalogRequest(tc.current, tc.update)
			doc := decodeCatalogBody(t, "chat", req)
			plain := req
			plain.Messages = []ChatMessage{req.Messages[0], req.Messages[2]}
			if want := decodeCatalogBody(t, "chat", plain); !reflect.DeepEqual(doc, want) {
				t.Fatalf("catalog metadata changed Chat request: %v", doc)
			}
			raw, _ := json.Marshal(doc["tools"])
			var got []ToolDef
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.current) {
				t.Fatalf("tools=%+v want=%+v", got, tc.current)
			}
		})
	}
}

func TestAnthropicIncrementalToolsRemovalAndRedefinition(t *testing.T) {
	base, added := catalogTool("base", "original"), catalogTool("late", "new")
	initial := catalogRequest([]ToolDef{base}, nil)
	initial.Messages = initial.Messages[:3]
	initialDoc := decodeCatalogBody(t, "anthropic", initial)
	for _, tc := range []struct {
		name    string
		current []ToolDef
		update  *messages.ToolUpdate
		types   []string
	}{
		{"add", []ToolDef{base, added}, &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{added})}, []string{"tool_addition"}},
		{"remove", nil, &messages.ToolUpdate{Removed: []string{"base"}}, []string{"tool_removal"}},
		{"replace", []ToolDef{added}, &messages.ToolUpdate{Removed: []string{"base"}, Added: toolDefinitions([]ToolDef{added})}, []string{"tool_removal", "tool_addition"}},
		{"redefine", []ToolDef{catalogTool("base", "changed")}, &messages.ToolUpdate{Removed: []string{"base"}, Added: toolDefinitions([]ToolDef{catalogTool("base", "changed")})}, []string{"tool_addition"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := catalogRequest(tc.current, tc.update)
			doc := decodeCatalogBody(t, "anthropic", req)
			if !reflect.DeepEqual(initialDoc["tools"], doc["tools"]) {
				t.Fatal("top-level declarations changed")
			}
			rows := doc["messages"].([]any)
			last := rows[len(rows)-1].(map[string]any)
			if last["role"] != "system" {
				t.Fatal(last)
			}
			blocks := last["content"].([]any)
			var types []string
			for _, block := range blocks {
				types = append(types, block.(map[string]any)["type"].(string))
			}
			if blocks[len(blocks)-1].(map[string]any)["cache_control"] == nil {
				t.Fatal("latest tool update has no cache breakpoint")
			}
			if !reflect.DeepEqual(types, tc.types) {
				t.Fatalf("types=%v want=%v", types, tc.types)
			}
			body, _ := json.Marshal(doc)
			if !strings.Contains(anthropicBetaHeader(body), anthropicInlineToolsBeta) {
				t.Fatal("missing inline beta")
			}
		})
	}
}

func TestAnthropicToolUpdateDoesNotSplitToolResultPair(t *testing.T) {
	base, added := catalogTool("base", "original"), catalogTool("late", "new")
	req := catalogRequest([]ToolDef{base, added}, &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{added})})
	update := req.Messages[3]
	req.Messages = append(req.Messages[:3], ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Function: Function{Name: "base", Arguments: "{}"}}}}, update, ChatMessage{Role: "tool", ToolCallID: "call", Content: "done"})
	rows := decodeCatalogBody(t, "anthropic", req)["messages"].([]any)
	var roles []string
	for _, row := range rows {
		roles = append(roles, row.(map[string]any)["role"].(string))
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "user", "system"}) {
		t.Fatal(roles)
	}
}

func TestToolProjectionAfterHistoryRewrite(t *testing.T) {
	tool := catalogTool("late", "new")
	req := CompletionRequest{Tools: []ToolDef{tool}, Messages: []ChatMessage{{Role: "system", ToolUpdate: &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{tool})}}}}
	if projectTools(req, toolUpdatesChanges).native {
		t.Fatal("orphan update accepted as baseline")
	}
	req.Messages = append(req.Messages, ChatMessage{Role: "system", ToolUpdate: &messages.ToolUpdate{Initial: true, Added: toolDefinitions(req.Tools)}})
	projection := projectTools(req, toolUpdatesChanges)
	if !projection.native || len(projection.updates) != 0 || len(projection.tools) != 1 {
		t.Fatalf("new baseline not used: %+v", projection)
	}
}

func TestToolUpdateFormatDependsOnlyOnWire(t *testing.T) {
	base, added := catalogTool("base", "original"), catalogTool("late", "new")
	req := catalogRequest([]ToolDef{base, added}, &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{added})})
	byWire := make(map[string]map[string]any)
	for _, tc := range []struct {
		name, wire, endpoint string
		native               bool
	}{
		{"responses", "responses", "https://api.openai.com/v1", true},
		{"responses gateway", "responses", "https://gateway.example/v1", true},
		{"anthropic", "anthropic", "https://api.anthropic.com", true},
		{"anthropic gateway", "anthropic", "https://gateway.example/v1", true},
		{"moonshot", "chat", "https://api.moonshot.ai/v1", false},
		{"moonshot cn", "chat", "https://api.moonshot.cn/v1", false},
		{"kimi coding", "chat", "https://api.kimi.com/coding/v1", false},
		{"openai chat", "chat", "https://api.openai.com/v1", false},
		{"chat gateway", "chat", "https://gateway.example/v1", false},
		{"multi-provider chat", "chat", "https://opencode.ai/zen/v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{BaseURL: tc.endpoint, Wire: tc.wire, APIModel: "deployment-alias"}
			doc := decodeCatalogProviderBody(t, cfg, req)
			if doc["model"] != cfg.APIModel {
				t.Fatal("registry model leaked onto wire")
			}
			if previous, ok := byWire[tc.wire]; ok && !reflect.DeepEqual(previous, doc) {
				t.Fatal("endpoint changed tool update format")
			}
			byWire[tc.wire] = doc
			// API model names are opaque. Renaming a deployment must change only
			// the model field, never the tool declarations or message format.
			cfg.APIModel = "another/model-name"
			renamed := decodeCatalogProviderBody(t, cfg, req)
			if renamed["model"] != cfg.APIModel {
				t.Fatal("API model was not forwarded")
			}
			renamed["model"] = doc["model"]
			if !reflect.DeepEqual(doc, renamed) {
				t.Fatal("model name changed tool update format")
			}
			key := "messages"
			if tc.wire == "responses" {
				key = "input"
			}
			hasUpdate := false
			for _, raw := range doc[key].([]any) {
				row := raw.(map[string]any)
				switch tc.wire {
				case "responses":
					hasUpdate = hasUpdate || row["type"] == "additional_tools"
				case "chat":
					hasUpdate = hasUpdate || row["tools"] != nil
				case "anthropic":
					for _, rawBlock := range row["content"].([]any) {
						hasUpdate = hasUpdate || rawBlock.(map[string]any)["type"] == "tool_addition"
					}
				}
			}
			if hasUpdate != tc.native {
				t.Fatalf("native update=%v want=%v: %v", hasUpdate, tc.native, doc)
			}
			topLevel, _ := json.Marshal(doc["tools"])
			if strings.Contains(string(topLevel), `"late"`) == tc.native {
				t.Fatalf("wrong top-level declarations: %s", topLevel)
			}
			if tc.wire == "anthropic" {
				body, _ := json.Marshal(doc)
				if strings.Contains(anthropicBetaHeader(body), anthropicInlineToolsBeta) != tc.native {
					t.Fatal("inline beta does not match selected format")
				}
			}
		})
	}
}

func TestToolUpdateBudgetRetainsRetiredSchemas(t *testing.T) {
	retired := catalogTool("old", strings.Repeat("schema description ", 1000))
	current := catalogTool("current", "small")
	req := CompletionRequest{Tools: []ToolDef{current}}
	full := EstimateRequestBudget(req, 0)
	req.Messages = []ChatMessage{
		{Role: "system", ToolUpdate: &messages.ToolUpdate{Initial: true, Added: toolDefinitions([]ToolDef{retired})}},
		{Role: "system", ToolUpdate: &messages.ToolUpdate{Removed: []string{"old"}, Added: toolDefinitions(req.Tools)}},
	}
	if got := EstimateRequestBudget(req, 0); got.SchemaTokens <= full.SchemaTokens+EstimateTokens(retired.Function.Description) {
		t.Fatalf("retired schema omitted from admission: %+v", got)
	}
}

func TestAnthropicToolUpdateHeadersWithoutAPIKey(t *testing.T) {
	var gotHeaders atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders.Store(r.Header.Clone())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()
	for _, key := range []string{"", "test-key"} {
		t.Run("key="+key, func(t *testing.T) {
			c, err := NewClient(Config{BaseURL: server.URL, Wire: "anthropic", APIKey: key})
			if err != nil {
				t.Fatal(err)
			}
			req := catalogRequest([]ToolDef{catalogTool("base", "original")}, nil)
			req.Messages = req.Messages[:3]
			body, err := c.marshalRequestBody(req)
			if err != nil {
				t.Fatal(err)
			}
			resp, _, _, err := c.postStream(context.Background(), server.URL+"/messages", body)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			headers := gotHeaders.Load().(http.Header)
			if headers.Get("anthropic-version") != "2023-06-01" || !strings.Contains(headers.Get("anthropic-beta"), anthropicInlineToolsBeta) {
				t.Fatalf("initial catalog missing protocol headers: %v", headers)
			}
			if headers.Get("x-api-key") != key {
				t.Fatal("authentication changed")
			}
		})
	}
}

func TestAnthropicToolPlaceholderCollisionFallsBack(t *testing.T) {
	base := catalogTool("base", "original")
	reserved := catalogTool(anthropicToolPlaceholder, "real tool")
	for _, where := range []string{"initial", "current", "transient"} {
		t.Run(where, func(t *testing.T) {
			req := catalogRequest([]ToolDef{base}, nil)
			switch where {
			case "initial":
				req.Messages[1].ToolUpdate.Added = toolDefinitions([]ToolDef{base, reserved})
				req.Messages[3].ToolUpdate = &messages.ToolUpdate{Removed: []string{reserved.Function.Name}}
			case "current":
				req.Tools = append(req.Tools, reserved)
				req.Messages[3].ToolUpdate = &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{reserved})}
			case "transient":
				req.Messages[3].ToolUpdate = &messages.ToolUpdate{Added: toolDefinitions([]ToolDef{reserved})}
				req.Messages = append(req.Messages, ChatMessage{Role: "system", ToolUpdate: &messages.ToolUpdate{Removed: []string{reserved.Function.Name}}})
			}
			doc := decodeCatalogBody(t, "anthropic", req)
			body, _ := json.Marshal(doc)
			if strings.Contains(anthropicBetaHeader(body), anthropicInlineToolsBeta) || strings.Contains(string(body), `"tool_addition"`) || strings.Contains(string(body), `"tool_removal"`) {
				t.Fatalf("placeholder collision did not fall back: %s", body)
			}
			if len(doc["tools"].([]any)) != len(req.Tools) {
				t.Fatal("fallback lost current tools")
			}
		})
	}
}
