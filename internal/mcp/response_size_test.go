package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestInvalidOrOversizedJSONReturnsImmediately(t *testing.T) {
	for _, body := range []string{"{invalid", strings.Repeat("x", maxLineLen+1)} {
		c := newHTTPTestClient(t, func(w http.ResponseWriter, r *http.Request, req rpcMessage) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
		done := make(chan error, 1)
		go func() { _, err := c.Call(context.Background(), "echo", nil); done <- err }()
		if err := receiveError(t, done); err == nil {
			t.Fatal("expected protocol/size error")
		}
	}
}
func TestReviewLargePlainJSONResponse(t *testing.T) {
	text := strings.Repeat("x", 1<<20)
	c := newHTTPTestClient(t, func(w http.ResponseWriter, r *http.Request, req rpcMessage) {
		writeRPC(w, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
	})
	out, err := c.Call(context.Background(), "echo", nil)
	if err != nil || out != text {
		t.Fatalf("response len=%d err=%v", len(out), err)
	}
}
