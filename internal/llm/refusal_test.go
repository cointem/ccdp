package llm

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestStreamRefusalPreservesVisibleAndFinalText(t *testing.T) {
	for _, tc := range []struct {
		name, wire, body, want string
		native                 bool
	}{
		{
			name: "chat",
			body: `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null,"refusal":"I cannot "}}]}

data: {"choices":[{"index":0,"delta":{"refusal":"help with that."}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`,
			want: "I cannot help with that.",
		},
		{
			name: "responses", wire: "responses", native: true,
			body: `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}

data: {"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"I cannot "}

data: {"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"help with that."}

data: {"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"I cannot help with that."}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I cannot help with that."}]}}

data: {"type":"response.completed","response":{"status":"completed"}}

`,
			want: "I cannot help with that.",
		},
		{
			name: "ordinary-chat-text",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"Answer\",\"refusal\":null},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			want: "Answer",
		},
		{
			name: "ordinary-responses-text", wire: "responses",
			body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
			want: "Answer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(Config{
				BaseURL: "https://example.invalid/v1", Wire: tc.wire,
				HTTPClient: &http.Client{Transport: auditTransport{tc.body}}, OneAttempt: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			var visible strings.Builder
			got, err := c.Stream(context.Background(), CompletionRequest{Model: "test"}, func(s string) {
				visible.WriteString(s)
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != tc.want || visible.String() != tc.want || got.FinishReason != "stop" {
				t.Fatalf("text=%q visible=%q finish=%q", got.Text, visible.String(), got.FinishReason)
			}
			if tc.native && (len(got.ProviderContent) != 1 || !strings.Contains(string(got.ProviderContent[0]), `"type":"refusal"`)) {
				t.Fatalf("native refusal was lost: %s", got.ProviderContent)
			}
		})
	}
}
