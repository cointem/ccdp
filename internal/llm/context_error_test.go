package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type contextRejectionTransport struct{ body string }

func (s contextRejectionTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{Status: "400 Bad Request", StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(s.body))}, nil
}

func TestContextOverflowRequiresStructuredProviderCode(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"error":{"code":"context_length_exceeded","message":"too long"}}`, true},
		{`{"error":{"type":"context_window_exceeded","message":"too long"}}`, true},
		{`{"error":{"type":"invalid_request_error","message":"context_length_exceeded in user input"}}`, false},
		{`plain context window exceeded`, false},
	} {
		client, err := NewClient(Config{BaseURL: "https://example.invalid", HTTPClient: &http.Client{Transport: contextRejectionTransport{tc.body}}, OneAttempt: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Stream(context.Background(), CompletionRequest{Model: "test"}, nil)
		if errors.Is(err, ErrContextWindowExceeded) != tc.want || !RejectedBeforeExecution(err) {
			t.Fatalf("wrong HTTP classification for %s: %v", tc.body, err)
		}
	}
}

func TestStreamContextOverflowPreservesTypedErrorAcrossWires(t *testing.T) {
	for _, tc := range []struct{ wire, event string }{
		{"", `{"error":{"code":"context_length_exceeded","message":"too long"}}`},
		{"responses", `{"type":"error","code":"context_length_exceeded","message":"too long"}`},
		{"responses", `{"type":"response.failed","response":{"status":"failed","error":{"code":"context_length_exceeded","message":"too long"}}}`},
		{"anthropic", `{"type":"error","error":{"type":"context_window_exceeded","message":"too long"}}`},
	} {
		client, err := NewClient(Config{BaseURL: "https://example.invalid", Wire: tc.wire,
			HTTPClient: &http.Client{Transport: auditTransport{"data: " + tc.event + "\n\n"}}, OneAttempt: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Stream(context.Background(), CompletionRequest{Model: "test"}, nil)
		if !errors.Is(err, ErrContextWindowExceeded) {
			t.Fatalf("wire %s lost context error: %v", tc.wire, err)
		}
	}
}
