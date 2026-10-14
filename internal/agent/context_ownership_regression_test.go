package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/tools"
)

type frozenSearchTool struct {
	name, description string
	schema            map[string]any
}

func (t *frozenSearchTool) Name() string                       { return t.name }
func (t *frozenSearchTool) Description() string                { return t.description }
func (t *frozenSearchTool) Parameters() map[string]any         { return t.schema }
func (t *frozenSearchTool) Run(*tools.Context) (string, error) { return "", nil }

func TestToolSearchDiscoveryUsesFrozenCatalogDefinition(t *testing.T) {
	a, p, _ := newJournalAgent(t, true)
	a.services.registry = tools.NewRegistry()
	a.discovery.discovered = map[string]bool{}
	tool := &frozenSearchTool{name: "candidate", description: "old needle", schema: map[string]any{"const": "old"}}
	a.services.registry.RegisterIn("mcp", tool)
	// Changing an extension's metadata after registration cannot change the
	// schema or description paired with the published implementation.
	tool.description = "new"
	tool.schema["const"] = "new"
	text, err := (&toolSearchTool{ag: a}).Run(&tools.Context{Args: map[string]any{"query": "needle"}})
	if err != nil || !strings.Contains(text, "old needle") {
		t.Fatalf("search=%q err=%v", text, err)
	}
	text, err = (&toolSearchTool{ag: a}).Run(&tools.Context{Args: map[string]any{"name": "candidate"}})
	if err != nil || !strings.Contains(text, "old needle") {
		t.Fatalf("discovery=%q err=%v", text, err)
	}
	records, err := p.Read(session.Beginning)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		var event session.ToolsDiscovered
		switch value := record.Event.(type) {
		case session.ToolsDiscovered:
			event = value
		case *session.ToolsDiscovered:
			event = *value
		default:
			continue
		}
		if len(event.Tools) != 1 || event.Tools[0].Description != "old needle" || !bytes.Contains(event.Tools[0].Schema, []byte(`"old"`)) {
			t.Fatalf("mixed discovery view: %+v", event)
		}
		return
	}
	t.Fatal("discovery was not recorded")
}

func TestHistoryAndSchemasOwnNestedJSON(t *testing.T) {
	a, _, _ := newJournalAgent(t, true)
	nested := map[string]any{"values": []any{map[string]any{"number": json.Number("9007199254740993"), "raw": json.RawMessage(`{"x":1}`), "bytes": []byte{1, 2}}}, "typed": []map[string]any{{"value": "before"}}}
	a.conversation.durable.History = []messages.Message{{Role: messages.RoleAssistant, ToolCalls: []messages.ToolCall{{ID: "copy-call", Arguments: nested}}, ProviderContent: []json.RawMessage{json.RawMessage(`{"type":"reasoning"}`)}, ImageAttachments: []messages.ImageAttachment{{Data: []byte{3, 4}}}}}
	a.conversation.durable.History = append(a.conversation.durable.History, messages.Message{Role: messages.RoleTool, ToolCallID: "copy-call", Content: "complete"})
	original := messages.Clone(a.conversation.durable.History)
	for _, history := range [][]messages.Message{a.History(), cloneForkHistory(a.conversation.durable.History)} {
		if len(history) == 0 {
			t.Fatal("complete fork history was dropped")
		}
		history[0].ToolCalls[0].Arguments["values"].([]any)[0].(map[string]any)["raw"].(json.RawMessage)[0] = '!'
		history[0].ToolCalls[0].Arguments["typed"].([]map[string]any)[0]["value"] = "after"
		history[0].ProviderContent[0][0] = '!'
		history[0].ImageAttachments[0].Data[0] = 9
	}
	if string(a.conversation.durable.History[0].ProviderContent[0]) != string(original[0].ProviderContent[0]) || a.conversation.durable.History[0].ImageAttachments[0].Data[0] != 3 || nested["typed"].([]map[string]any)[0]["value"] != "before" {
		t.Fatal("history shares nested mutable storage")
	}
	defs := cloneToolDefs([]llm.ToolDef{{Function: llm.FuncDef{Parameters: nested}}})
	values := defs[0].Function.Parameters["values"].([]any)[0].(map[string]any)
	if values["number"] != json.Number("9007199254740993") {
		t.Fatalf("numeric precision changed: %v", values["number"])
	}
	values["bytes"].([]byte)[0] = 9
	if nested["values"].([]any)[0].(map[string]any)["bytes"].([]byte)[0] != 1 {
		t.Fatal("schema shares bytes")
	}
}

func TestActiveFallbackControlsCompactionAndSummaryBudget(t *testing.T) {
	var captured llm.CompletionRequest
	provider := &childRuntimeTestProvider{name: "small-summary", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		captured = req
		return llm.StreamResult{Text: "summary", FinishReason: "stop"}, nil
	}}
	a := newJournalRuntimeAgent(t, provider)
	a.services.cfg.Providers = map[string]config.ProviderConfig{"router": {Models: []string{a.services.cfg.Model, "small"}, ContextWindows: map[string]int{a.services.cfg.Model: 200000, "small": 2048}}}
	a.modelsState.activeBinding.model = "small"
	a.modelsState.activeModel = "small"
	a.conversation.durable.History = []messages.Message{{Role: messages.RoleUser, Content: strings.Repeat("old long context ", 10000)}, {Role: messages.RoleUser, Content: "latest input"}}
	if !a.needsCompact() {
		t.Fatal("fallback budget did not trigger compaction")
	}
	before := a.compactFailureKeyLocked()
	a.services.cfg.Providers["router"] = config.ProviderConfig{Models: []string{a.services.cfg.Model, "small"}, ContextWindows: map[string]int{a.services.cfg.Model: 200000, "small": 1024}}
	if before == a.compactFailureKeyLocked() {
		t.Fatal("failure state ignores active budget")
	}
	if err := a.compactContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	caps := llm.Capabilities{ContextWindow: 1024, SystemRole: true, Reasoning: true}
	if _, err := llm.AdmitRequest(&capabilityTestProviderForContext{childRuntimeTestProvider: provider, caps: caps}, captured); err != nil {
		t.Fatalf("summary exceeds fallback budget: %v", err)
	}
	view, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Settings.ContextWindow != 1024 {
		t.Fatalf("context status window=%d", view.Settings.ContextWindow)
	}
}

func TestAttachmentBudgetCompactsShortHistoryAndNextTextTurn(t *testing.T) {
	var calls atomic.Int32
	provider := &childRuntimeTestProvider{name: "image-summary", stream: func(_ context.Context, req llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
		calls.Add(1)
		return llm.StreamResult{Text: "Earlier image analysis is archived in the session record.", FinishReason: "stop"}, nil
	}}
	a := newJournalRuntimeAgent(t, provider)
	for i := 0; i < 3; i++ {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, i+1, i+1))); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 16<<20)
		copy(data, encoded.Bytes())
		data[len(data)-1] = byte(i + 1)
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "![capture](capture.png)", ImagesFrozen: true, ImageAttachments: []messages.ImageAttachment{{Path: "capture.png", MediaType: "image/png", Data: data}}}); err != nil {
			t.Fatal(err)
		}
	}
	if !a.needsCompact() {
		t.Fatal("attachment budget did not trigger compaction")
	}
	if _, err := a.buildRequestForStepChecked(stepRuntime{cfg: a.services.cfg.Clone(), history: a.History()}); err == nil {
		t.Fatal("low-level attachment guard accepted an oversized request")
	}
	if err := a.compactContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	history := a.History()
	if historyAttachmentBytes(history) > maxAttachmentBytes {
		t.Fatal("compaction left oversized image context")
	}
	newest := history[len(history)-1]
	if len(newest.ImageAttachments) != 1 {
		t.Fatal("latest user attachment was removed")
	}
	data, err := a.frozenImageBytes(newest.ImageAttachments[0])
	if err != nil {
		t.Fatal(err)
	}
	if data[len(data)-1] != 3 {
		t.Fatal("latest user image changed")
	}
	// Add another individually admissible image so the next text turn must
	// compact automatically before constructing its request.
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "![capture](capture.png)", ImagesFrozen: true, ImageAttachments: []messages.ImageAttachment{{Path: "capture.png", MediaType: "image/png", Data: data}}}); err != nil {
		t.Fatal(err)
	}
	// Race instrumentation also serializes several large wire/blob copies.
	// Give this 48 MiB fixture its own wait budget; ordinary turns retain 10s.
	formalCompletionTurn(t, a, "image-budget-text", "continue with text", 60*time.Second)
	view, _ := a.Snapshot(context.Background())
	if view.LastTurn.Status != protocol.TurnSucceeded {
		t.Fatalf("text turn remains blocked: %+v", view.LastTurn)
	}
	if calls.Load() < 3 {
		t.Fatal("subsequent text turn did not call model")
	}
}

func TestPreCompactContinueFalseStopsAutomaticTurn(t *testing.T) {
	var calls atomic.Int32
	provider := &childRuntimeTestProvider{name: "hook-stop", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		calls.Add(1)
		return llm.StreamResult{Text: "should not run", FinishReason: "stop"}, nil
	}}
	a := newJournalRuntimeAgent(t, provider)
	a.services.cfg.ContextWindow = 2048
	a.services.hooks = hooks.NewManager(hooks.Config{hooks.EventPreCompact: []hooks.HookSpec{{Command: `printf '%s' '{"continue":false,"reason":"stop compact"}'`}}}, hooks.Options{Workspace: a.services.cfg.Workspace, Sandbox: a.services.sandbox, FailClosed: true})
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: strings.Repeat("long history ", 100000)}); err != nil {
		t.Fatal(err)
	}
	formalCompletionTurn(t, a, "precompact-stop", "new request")
	if calls.Load() != 0 {
		t.Fatal("precompact stop continued into model call")
	}

}

func TestReviewAndMemoryDoNotReadLegacyFiles(t *testing.T) {
	for _, memory := range []bool{true, false} {
		t.Run(map[bool]string{true: "memory", false: "disk"}[memory], func(t *testing.T) {
			a, _, cfg := newJournalAgent(t, memory)
			id := "old-review"
			dir := filepath.Join(cfg.SessionDir, a.SessionID(), "reviews")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, base64.RawURLEncoding.EncodeToString([]byte(id))+".md"), []byte("old report"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.SessionDir, a.SessionID()+"-memory.md"), []byte("old memory"), 0600); err != nil {
				t.Fatal(err)
			}
			if report, err := a.readReviews(&protocol.WorkflowCommand{Action: "report", ID: id}); err == nil {
				t.Fatalf("read legacy report: %q", report)
			}
			if text := a.MemoryText(); text != "" {
				t.Fatalf("read legacy memory: %q", text)
			}
		})
	}
}

type capabilityTestProviderForContext struct {
	*childRuntimeTestProvider
	caps llm.Capabilities
}

func (p *capabilityTestProviderForContext) Capabilities() llm.Capabilities { return p.caps }

func TestHookContinueFalseStopsDecisionAndObserverBoundaries(t *testing.T) {
	for _, stage := range []string{hooks.EventUserPromptSubmit, hooks.EventPreToolUse, hooks.EventPostToolUse} {
		t.Run(stage, func(t *testing.T) {
			var calls atomic.Int32
			provider := &childRuntimeTestProvider{name: "hook-boundary-stop", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
				if calls.Add(1) == 1 {
					return llm.StreamResult{ToolCalls: wireToolCalls([]messages.ToolCall{{ID: "stop-read", Name: "Read", Arguments: map[string]any{"file_path": "input.txt"}}}), FinishReason: "tool_calls"}, nil
				}
				return llm.StreamResult{Text: "should not continue", FinishReason: "stop"}, nil
			}}
			a := newJournalRuntimeAgent(t, provider)
			if err := os.WriteFile(filepath.Join(a.services.cfg.Workspace, "input.txt"), []byte("context"), 0600); err != nil {
				t.Fatal(err)
			}
			later := `printf '%s' '{"decision":"allow","reason":"later allow"}'`
			if stage == hooks.EventUserPromptSubmit {
				later = `printf '%s' '{"decision":"deny","reason":"later deny"}'`
			}
			a.services.hooks = hooks.NewManager(hooks.Config{stage: []hooks.HookSpec{{Command: `printf '%s' '{"continue":false,"reason":"stop"}'`}, {Command: later}}}, hooks.Options{Workspace: a.services.cfg.Workspace, Sandbox: a.services.sandbox, FailClosed: true})
			formalCompletionTurn(t, a, "hook-boundary-stop", "inspect input.txt")
			want := int32(1)
			if stage == hooks.EventUserPromptSubmit {
				want = 0
			}
			if calls.Load() != want {
				t.Fatalf("%s continued into %d model calls, want %d", stage, calls.Load(), want)
			}
			if !a.interrupted() {
				t.Fatalf("%s did not stop the live turn", stage)
			}
		})
	}
}
