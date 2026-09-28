package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type largeResponseTransport struct{}

type bodyResponseTransport struct{ body string }

func (r bodyResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(r.body))}, nil
}

func TestInvalidOrOversizedJSONReturnsImmediately(t *testing.T) {
	for _, body := range []string{"{invalid", strings.Repeat("x", maxLineLen+1)} {
		c := NewClient("test", ServerConfig{Transport: "sse"})
		c.sseTransport = true
		c.messagesURL = "http://example.test/messages"
		c.sseCtx = context.Background()
		c.sseClient = &http.Client{Transport: bodyResponseTransport{body}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var out map[string]any
		err := c.requestJSON(ctx, "tools/call", map[string]any{}, &out)
		cancel()
		if err == nil || strings.Contains(err.Error(), "deadline") {
			t.Fatalf("expected immediate protocol error: %v", err)
		}
	}
}

func (largeResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"content":"%s"}}`, strings.Repeat("x", 1<<20))
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestReviewLargePlainJSONResponse(t *testing.T) {
	c := NewClient("test", ServerConfig{Transport: "sse"})
	c.sseTransport = true
	c.messagesURL = "http://example.test/messages"
	c.sseClient = &http.Client{Transport: largeResponseTransport{}}
	c.sseCtx = context.Background()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var out map[string]any
	if err := c.requestJSON(ctx, "tools/call", map[string]any{}, &out); err != nil {
		t.Fatalf("valid response failed: %v", err)
	}
}
