package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type auditTransport struct{ body string }

func (a auditTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(a.body))}, nil
}

func TestStreamBuffersPreserveDeltasOnSuccessAndError(t *testing.T) {
	for _, wire := range []struct{ name, reasoning, text, end, failure string }{
		{"chat", `{"choices":[{"delta":{"reasoning_content":"思考"}}]}`, `{"choices":[{"delta":{"content":"回答"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `{"error":{"message":"broken"}}`},
		{"responses", `{"type":"response.reasoning_summary_text.delta","delta":"思考"}`, `{"type":"response.output_text.delta","delta":"回答"}`, `{"type":"response.completed","response":{"status":"completed"}}`, `{"type":"error","message":"broken"}`},
		{"anthropic", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"思考"}}`, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"回答"}}`, `{"type":"message_stop"}`, `{"type":"error","error":{"message":"broken"}}`},
	} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/error=%t", wire.name, fail), func(t *testing.T) {
				frame := func(s string) string { return "data: " + s + "\n\n" }
				end := wire.end
				if fail {
					end = wire.failure
				}
				body := strings.Repeat(frame(wire.reasoning), 4096) + strings.Repeat(frame(wire.text), 4096) + frame(end)
				name := wire.name
				if name == "chat" {
					name = ""
				}
				c, err := NewClient(Config{BaseURL: "https://example.invalid/v1", Wire: name, HTTPClient: &http.Client{Transport: auditTransport{body}}, OneAttempt: true})
				if err != nil {
					t.Fatal(err)
				}
				var text, reasoning strings.Builder
				r, err := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "test"}, func(s string) { text.WriteString(s) }, func(s string) { reasoning.WriteString(s) })
				if (err != nil) != fail {
					t.Fatalf("error=%v, want failure=%t", err, fail)
				}
				if r.Text != strings.Repeat("回答", 4096) || r.Reasoning != strings.Repeat("思考", 4096) || r.Text != text.String() || r.Reasoning != reasoning.String() {
					t.Fatalf("buffered result differs from delivered deltas: text=%d reasoning=%d", len(r.Text), len(r.Reasoning))
				}
			})
		}
	}
}

func BenchmarkStreamReasoning(b *testing.B) {
	for _, size := range []int{16 << 10, 64 << 10, 256 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			body := strings.Repeat("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\""+strings.Repeat("x", 32)+"\"}}]}\n\n", size/32) + "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
			c, err := NewClient(Config{BaseURL: "https://example.invalid/v1", HTTPClient: &http.Client{Transport: auditTransport{body}}, OneAttempt: true})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, e := c.StreamWithReasoning(context.Background(), CompletionRequest{Model: "test"}, nil, func(string) {})
				if e != nil || len(r.Reasoning) != size {
					b.Fatal(e, len(r.Reasoning))
				}
			}
		})
	}
}
