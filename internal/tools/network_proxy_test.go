package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebProxyUsesUpstreamDNSAndChecksRedirects(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "http://127.0.0.1/private")
			w.WriteHeader(302)
			return
		}
		fmt.Fprint(w, "resolved upstream")
	}))
	defer proxy.Close()
	body, _, _, _, err := httpGetDetails(context.Background(), "http://public.invalid/", proxy.URL)
	if err != nil || string(body) != "resolved upstream" {
		t.Fatalf("proxy fetch: %q %v", body, err)
	}
	_, _, _, _, err = httpGetDetails(context.Background(), "http://public.invalid/redirect", proxy.URL)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("redirect escaped policy: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("private redirect reached proxy")
	}
	// Exercise the public tool entry point, not only its HTTP helper.
	ctx := rgCtx(t.TempDir())
	ctx.HostNetworkAllowed = true
	ctx.Sandbox.NetworkProxy = proxy.URL
	ctx.Args = map[string]any{"url": "http://public.invalid/"}
	out, err := NewWebFetchTool().Run(ctx)
	if err != nil || !strings.Contains(out, "resolved upstream") {
		t.Fatalf("WebFetch proxy wiring: %q %v", out, err)
	}
	ctx.HostNetworkAllowed = false
	if _, err := NewWebFetchTool().Run(ctx); err == nil {
		t.Fatal("proxy bypassed network permission")
	}
}
