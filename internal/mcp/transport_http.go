package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"ccdp/internal/netguard"
)

// Pin a supported stateful protocol: newer SDK defaults can change the handshake.
const httpProtocolVersion = "2025-11-25"

func (c *Client) startSSE(ctx context.Context) error {
	transport, err := netguard.OriginTransport(c.cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("mcp %s: %w", c.name, err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	origin, _ := url.Parse(c.cfg.BaseURL)
	client := &http.Client{
		Transport:     &mcpHTTPTransport{base: transport, origin: origin, lifetime: lifetime},
		CheckRedirect: sameSSEOriginRedirect,
	}
	c.mu.Lock()
	c.sseClient, c.sessionCancel, c.sessionCtx = client, cancel, lifetime
	c.mu.Unlock()
	stopStartup := context.AfterFunc(ctx, cancel)
	defer stopStartup()
	if c.isClosed() {
		cancel()
		return fmt.Errorf("mcp %s: client closed", c.name)
	}
	remote := c.sdkClient()
	connect := func(t sdk.Transport) (*sdk.ClientSession, error) {
		// Legacy GET must outlive startup; the startup cancellation hook still
		// interrupts a stalled GET/handshake before ownership passes to Close.
		return remote.Connect(ctx, lifetimeTransport{Transport: t, lifetime: lifetime}, &sdk.ClientSessionOptions{ProtocolVersion: httpProtocolVersion})
	}
	session, err := connect(&sdk.StreamableClientTransport{Endpoint: c.cfg.BaseURL, HTTPClient: client, MaxEventSize: maxLineLen})
	var rejected *httpStatusError
	if errors.As(err, &rejected) && (rejected.code == 404 || rejected.code == 405) {
		session, err = connect(&sdk.SSEClientTransport{Endpoint: c.cfg.BaseURL, HTTPClient: client, MaxEventSize: maxLineLen})
	}
	if err != nil {
		_ = c.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return fmt.Errorf("mcp %s: connect: %w", c.name, err)
	}
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	go func() { _ = session.Wait(); c.fireClosed() }()
	if c.isClosed() {
		_ = c.Close()
		return fmt.Errorf("mcp %s: closed during startup", c.name)
	}
	var list struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := c.requestJSON(ctx, "tools/list", nil, &list); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp %s: tools/list: %w", c.name, err)
	}
	c.mu.Lock()
	c.tools = list.Tools
	c.mu.Unlock()
	if !stopStartup() || ctx.Err() != nil || c.isClosed() {
		_ = c.Close()
		return fmt.Errorf("mcp %s: startup cancelled or connection closed", c.name)
	}
	return nil
}

type lifetimeTransport struct {
	sdk.Transport
	lifetime context.Context
}

func (t lifetimeTransport) Connect(context.Context) (sdk.Connection, error) {
	return t.Transport.Connect(t.lifetime)
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP status %d", e.code) }

// Keep ccdp's network and response-size boundaries around the SDK. Check every
// URL (including legacy endpoint events), even when HTTP reuses a connection.
type mcpHTTPTransport struct {
	base     http.RoundTripper
	origin   *url.URL
	lifetime context.Context
}

func (t *mcpHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !sameHTTPOrigin(t.origin, req.URL) {
		return nil, fmt.Errorf("MCP endpoint crosses the configured origin")
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := func() bool { return true }
	// Session termination is best-effort and separately bounded by the SDK;
	// it must still be sent after active calls and streams have been cancelled.
	if req.Method != http.MethodDelete {
		stop = context.AfterFunc(t.lifetime, cancel)
		if t.lifetime.Err() != nil {
			stop()
			cancel()
			return nil, t.lifetime.Err()
		}
	}
	release := func() { stop(); cancel() }
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		release()
		return nil, err
	}
	if req.Method == http.MethodPost && req.Header.Get("Mcp-Session-Id") == "" && req.Header.Get("Mcp-Protocol-Version") == "" && (resp.StatusCode == 404 || resp.StatusCode == 405) {
		resp.Body.Close()
		release()
		return nil, &httpStatusError{code: resp.StatusCode}
	}
	body := resp.Body
	// JSON is a finite body; event streams are bounded per event by the SDK.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		body = &limitedMCPBody{ReadCloser: body, remaining: maxLineLen}
	}
	resp.Body = &lifetimeBody{ReadCloser: body, release: release}
	return resp, nil
}
func (t *mcpHTTPTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

type lifetimeBody struct {
	io.ReadCloser
	release func()
}

func (b *lifetimeBody) Close() error { err := b.ReadCloser.Close(); b.release(); return err }

type limitedMCPBody struct {
	io.ReadCloser
	remaining int
}

func (b *limitedMCPBody) Read(p []byte) (int, error) {
	if len(p) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	if b.remaining < 0 {
		return 0, fmt.Errorf("MCP response exceeds %d bytes", maxLineLen)
	}
	return n, err
}
func sameSSEOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("mcp sse: stopped after 10 redirects")
	}
	if len(via) == 0 || req.URL == nil || !sameHTTPOrigin(via[0].URL, req.URL) {
		return fmt.Errorf("mcp sse: redirect crosses the configured origin")
	}
	return nil
}

func sameHTTPOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return port(a) == port(b)
}
