package llm

import (
	"context"
	"net/http"
	"testing"
)

func TestRegressionAnthropicRealStopReason(t *testing.T) {
	body := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":100}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	c, err := NewClient(Config{BaseURL: "https://example.invalid/v1", Wire: "anthropic", HTTPClient: &http.Client{Transport: auditTransport{body}}, OneAttempt: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Stream(context.Background(), CompletionRequest{Model: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.FinishReason != "length" {
		t.Fatalf("real message_delta lost stop reason: %q", r.FinishReason)
	}
}

func TestRegressionAnthropicPrematureEOF(t *testing.T) {
	body := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"half an answer\"}}\n\n"
	c, err := NewClient(Config{BaseURL: "https://example.invalid/v1", Wire: "anthropic", HTTPClient: &http.Client{Transport: auditTransport{body}}, OneAttempt: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Stream(context.Background(), CompletionRequest{Model: "test"}, nil)
	if err == nil {
		t.Fatalf("premature EOF reported success: %+v", r)
	}
}

func TestRegressionResponsesPrematureEOF(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"half an answer\"}\n\n"
	c, err := NewClient(Config{BaseURL: "https://example.invalid/v1", Wire: "responses", HTTPClient: &http.Client{Transport: auditTransport{body}}, OneAttempt: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Stream(context.Background(), CompletionRequest{Model: "test"}, nil)
	if err == nil {
		t.Fatalf("premature EOF reported success: %+v", r)
	}
}
