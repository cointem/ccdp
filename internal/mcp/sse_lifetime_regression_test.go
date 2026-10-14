package mcp

import (
	"context"
	"testing"
	"time"
)

func TestReviewRefreshSSESurvivesOperationCompletion(t *testing.T) {
	srv := newLegacyTestServer(t, nil, "echo")
	defer srv.Close()
	m := NewManager()
	defer m.Close()
	operationCtx, finishOperation := context.WithCancel(context.Background())
	defer finishOperation()
	candidate, err := m.PrepareRefresh(operationCtx, map[string]ServerConfig{"remote": {Transport: "sse", BaseURL: srv.URL, NetworkAuthorized: true}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	c := candidate.clients[0].client
	if err = candidate.Commit(); err != nil {
		t.Fatal(err)
	}
	finishOperation()
	select {
	case <-c.closed:
		t.Fatal("published SSE client is closed by successful reload operation completion")
	case <-time.After(100 * time.Millisecond):
	}
}
