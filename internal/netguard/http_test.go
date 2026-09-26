package netguard

import (
	"context"
	"strings"
	"testing"
)

func TestPublicTransportRejectsLoopbackAtActualDial(t *testing.T) {
	transport := PublicTransport()
	if transport.Proxy != nil {
		t.Fatal("public HTTP transport unexpectedly uses a host proxy")
	}
	_, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "internal/private") {
		t.Fatalf("public dial to loopback = %v", err)
	}
}

func TestOriginTransportIsDirectAndScopedToConfiguredHostPort(t *testing.T) {
	transport, err := OriginTransport("http://example.test/sse")
	if err != nil {
		t.Fatal(err)
	}
	if transport.Proxy != nil {
		t.Fatal("origin transport unexpectedly uses a host proxy")
	}
	_, err = transport.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil || !strings.Contains(err.Error(), "outside the configured origin") {
		t.Fatalf("dial outside configured MCP origin = %v", err)
	}

}
