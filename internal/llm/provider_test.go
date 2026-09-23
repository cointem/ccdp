package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

type preparedTestProvider struct {
	name string
	got  CompletionRequest
}

func (p *preparedTestProvider) Name() string { return p.name }
func (p *preparedTestProvider) Stream(_ context.Context, req CompletionRequest, _ func(string)) (StreamResult, error) {
	p.got = req
	return StreamResult{Text: "answer", ToolCalls: []ToolCall{{ID: "call-1"}}}, nil
}

type capabilityTestProvider struct {
	name  string
	caps  Capabilities
	calls atomic.Int32
}

func (p *capabilityTestProvider) Name() string               { return p.name }
func (p *capabilityTestProvider) Capabilities() Capabilities { return p.caps }
func (p *capabilityTestProvider) Stream(context.Context, CompletionRequest, func(string)) (StreamResult, error) {
	p.calls.Add(1)
	return StreamResult{Text: "should not be called"}, nil
}

type boundedPreparedTestProvider struct {
	preparedTestProvider
	caps Capabilities
}

func (p *boundedPreparedTestProvider) Capabilities() Capabilities { return p.caps }

func TestPreparedCallFreezesSerializableRequestAndStreamFlag(t *testing.T) {
	provider := &preparedTestProvider{name: "fake"}
	params := map[string]any{"nested": map[string]any{"value": "before"}}
	req := CompletionRequest{
		Model:    "model-a",
		Messages: []ChatMessage{{Role: "user", Content: "before"}},
		Tools:    []ToolDef{{Type: "function", Function: FuncDef{Name: "Tool", Parameters: params}}},
		Stream:   false,
	}
	call, err := NewPreparedCall(provider, provider.Name(), "", req)
	if err != nil {
		t.Fatal(err)
	}
	req.Messages[0].Content = "after"
	params["nested"].(map[string]any)["value"] = "after"

	manifest := call.Manifest()
	var wire CompletionRequest
	if err := json.Unmarshal(manifest.RequestJSON, &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.Stream || wire.Model != "model-a" {
		t.Fatalf("manifest request = %+v", wire)
	}
	if _, err := call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if provider.got.Stream != true || provider.got.Messages[0].Content != "before" {
		t.Fatalf("provider received unfrozen request: %+v", provider.got)
	}
	if provider.got.Tools[0].Function.Parameters["nested"].(map[string]any)["value"] != "before" {
		t.Fatalf("nested schema changed: %+v", provider.got.Tools[0].Function.Parameters)
	}
}

func TestPreparedCallCopiesResult(t *testing.T) {
	provider := &preparedTestProvider{name: "fake"}
	call, err := NewPreparedCall(provider, "fake", "", CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := call.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result.ToolCalls[0].ID = "mutated"
	second, err := call.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.ToolCalls[0].ID != "call-1" {
		t.Fatalf("provider result leaked through prepared call: %+v", second.ToolCalls)
	}
}

func TestPreparedCallRejectsCyclicRequest(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	_, err := NewPreparedCall(&preparedTestProvider{name: "fake"}, "fake", "", CompletionRequest{
		Model: "m", Messages: []ChatMessage{{Role: "user", Content: cyclic}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("expected controlled cycle error, got %v", err)
	}
}

func TestPreparedRequestPreservesUnknownArrayContent(t *testing.T) {
	provider := &preparedTestProvider{name: "fake"}
	req := CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: []map[string]any{{"type": "custom", "extra": map[string]any{"x": 1}}}}}}
	call, err := NewPreparedCall(provider, "fake", "", req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(provider.got.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "extra") || !strings.Contains(string(raw), "custom") {
		t.Fatalf("unknown array content was dropped: %s", raw)
	}
}

func TestPreparedCallNormalizesCustomContentToProviderWire(t *testing.T) {
	provider := &preparedTestProvider{name: "wire"}
	type customPart struct {
		Type    string      `json:"type"`
		Payload string      `json:"payload"`
		Count   json.Number `json:"count"`
	}
	part := &customPart{Type: "custom", Payload: "before", Count: json.Number("9007199254740993")}
	call, err := NewPreparedCall(provider, provider.Name(), "", CompletionRequest{
		Model:    "wire",
		Messages: []ChatMessage{{Role: "user", Content: []any{part}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	manifest := call.Manifest()
	part.Payload = "after"
	if _, err := call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	sent, err := json.Marshal(provider.got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sent, manifest.RequestJSON) {
		t.Fatalf("provider wire differs from frozen manifest:\nmanifest=%s\nsent=%s", manifest.RequestJSON, sent)
	}
	if !strings.Contains(string(manifest.RequestJSON), "9007199254740993") || strings.Contains(string(manifest.RequestJSON), "after") {
		t.Fatalf("custom content was not frozen without losing the large integer: %s", manifest.RequestJSON)
	}
}

func TestPreparedCallAdmissionRunsBeforeProvider(t *testing.T) {
	tests := []struct {
		name string
		caps Capabilities
		req  CompletionRequest
		want error
	}{
		{
			name: "tools",
			caps: Capabilities{SystemRole: true},
			req:  CompletionRequest{Model: "m", Tools: []ToolDef{{Type: "function", Function: FuncDef{Name: "read"}}}},
			want: ErrCapabilityUnsupported,
		},
		{
			name: "images including empty URL",
			caps: Capabilities{SystemRole: true},
			req: CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: ContentPart{
				Type: "image_url", ImageURL: &struct {
					URL string `json:"url"`
				}{},
			}}}},
			want: ErrCapabilityUnsupported,
		},
		{
			name: "system",
			caps: Capabilities{},
			req:  CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "system", Content: "instructions"}}},
			want: ErrCapabilityUnsupported,
		},
		{
			name: "output ceiling",
			caps: Capabilities{MaxOutputTokens: 4},
			req:  CompletionRequest{Model: "m", MaxTokens: intPtrForTest(5)},
			want: ErrMaxOutputExceeded,
		},
		{
			name: "context window",
			caps: Capabilities{ContextWindow: 8, SystemRole: true},
			req:  CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: strings.Repeat("x", 40)}}, MaxTokens: intPtrForTest(1)},
			want: ErrContextWindowExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &capabilityTestProvider{name: "cap-" + test.name, caps: test.caps}
			if _, err := NewPreparedCall(provider, provider.Name(), "", test.req); !errors.Is(err, test.want) {
				t.Fatalf("NewPreparedCall error = %v, want errors.Is(%v)", err, test.want)
			}
			if got := provider.calls.Load(); got != 0 {
				t.Fatalf("provider Stream called during rejected preparation: %d", got)
			}
		})
	}
}

func TestPreparedCallAdmissionKeepsUndescribedProviderCompatible(t *testing.T) {
	provider := &preparedTestProvider{name: "legacy"}
	call, err := NewPreparedCall(provider, provider.Name(), "", CompletionRequest{
		Model: "m",
		Messages: []ChatMessage{{Role: "system", Content: "instructions"}, {
			Role: "user", Content: "run it",
		}},
		Tools: []ToolDef{{Type: "function", Function: FuncDef{Name: "run"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	if _, err := call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedCallKeepsEstimatedReservationOffWire(t *testing.T) {
	provider := &boundedPreparedTestProvider{
		preparedTestProvider: preparedTestProvider{name: "bounded"},
		caps:                 Capabilities{ContextWindow: 100, SystemRole: true},
	}
	call, err := NewPreparedCall(provider, provider.Name(), "", CompletionRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	wire, err := PreparedRequest(call)
	if err != nil {
		t.Fatal(err)
	}
	if wire.MaxTokens != nil {
		t.Fatalf("estimated reservation leaked onto wire: %v", wire.MaxTokens)
	}
	budget, err := AdmitRequest(provider, wire)
	if err != nil {
		t.Fatal(err)
	}
	if budget.ReservedOutputTokens != 25 {
		t.Fatalf("admission output reservation = %d, want 25", budget.ReservedOutputTokens)
	}
	if _, err := call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if provider.got.MaxTokens != nil {
		t.Fatalf("provider received implicit max_tokens = %v", provider.got.MaxTokens)
	}
}

func TestAdmitRequestDefaultOutputHonorsProviderCeiling(t *testing.T) {
	provider := &capabilityTestProvider{name: "bounded", caps: Capabilities{ContextWindow: 100, MaxOutputTokens: 3}}
	budget, err := AdmitRequest(provider, CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if budget.ReservedOutputTokens != 3 {
		t.Fatalf("default output reservation = %d, want provider ceiling 3", budget.ReservedOutputTokens)
	}
}

func TestAdmitRequestDefaultOutputLeavesRoomOnTinyWindow(t *testing.T) {
	provider := &capabilityTestProvider{name: "tiny", caps: Capabilities{ContextWindow: 4}}
	budget, err := AdmitRequest(provider, CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if budget.ReservedOutputTokens != 1 {
		t.Fatalf("default output reservation = %d, want one token on tiny window", budget.ReservedOutputTokens)
	}
}

func TestAdmitRequestRejectsNegativeOutputForLegacyProvider(t *testing.T) {
	provider := &preparedTestProvider{name: "legacy"}
	if _, err := AdmitRequest(provider, CompletionRequest{Model: "m", MaxTokens: intPtrForTest(-1)}); !errors.Is(err, ErrMaxOutputExceeded) {
		t.Fatalf("legacy negative max_tokens error = %v, want ErrMaxOutputExceeded", err)
	}
}

func TestEstimateRequestBudgetCountsWireMetadataAndBoundsImages(t *testing.T) {
	largeDataURL := "data:image/png;base64," + strings.Repeat("A", 2<<20)
	req := CompletionRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: "system", Content: "system instructions", Name: "system-name"},
			{Role: "tool", Content: "tool result", ToolCallID: "call-1"},
			{Role: "user", Content: map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": largeDataURL},
			}},
		},
		Tools:     []ToolDef{{Type: "function", Function: FuncDef{Name: "lookup", Parameters: map[string]any{"type": "object"}}}},
		MaxTokens: intPtrForTest(17),
	}
	budget := EstimateRequestBudget(req, 4096)
	if budget.SystemTokens == 0 || budget.SchemaTokens == 0 || budget.HistoryTokens == 0 {
		t.Fatalf("budget omitted wire sections: %+v", budget)
	}
	if budget.ImageTokens != defaultImageTokenReserve {
		t.Fatalf("image budget = %d, want bounded reserve %d", budget.ImageTokens, defaultImageTokenReserve)
	}
	if budget.MessageMetadataTokens < EstimateTokens("system")+EstimateTokens("system-name")+EstimateTokens("tool")+EstimateTokens("call-1") {
		t.Fatalf("message metadata did not include role/name/tool-result id: %+v", budget)
	}
	if budget.EnvelopeTokens != 2+len(req.Messages)*3+len(req.Tools)*2 {
		t.Fatalf("envelope budget = %d, want fixed wire envelope", budget.EnvelopeTokens)
	}
	if budget.TotalTokens != budget.InputTokens+17 {
		t.Fatalf("total budget = %+v", budget)
	}
}

func TestAdmitRequestProviderImageReserveOverridesDefault(t *testing.T) {
	provider := &capabilityTestProvider{name: "image-cap", caps: Capabilities{Images: true, ImageTokenReserve: 9}}
	req := CompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: ContentPart{Type: "image"}}}}
	budget, err := AdmitRequest(provider, req)
	if err != nil {
		t.Fatal(err)
	}
	if budget.ImageTokens != 9 {
		t.Fatalf("provider image reserve = %d, want 9", budget.ImageTokens)
	}
}

func intPtrForTest(value int) *int { return &value }

func TestPreparedCallPreservesExplicitOutputCap(t *testing.T) {
	provider := &boundedPreparedTestProvider{preparedTestProvider: preparedTestProvider{name: "bounded"}, caps: Capabilities{ContextWindow: 1000000, Reasoning: true}}
	cap := 32768
	call, err := NewPreparedCall(provider, provider.Name(), "", CompletionRequest{Model: "m", MaxTokens: &cap})
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	if _, err := call.Stream(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if provider.got.MaxTokens == nil || *provider.got.MaxTokens != cap {
		t.Fatalf("explicit cap changed: %v", provider.got.MaxTokens)
	}
}
