// Package mcp implements a Model Context Protocol (MCP) client over stdio,
// mirroring the MCP support in Claude Code and Codex. Each configured server is
// a child process speaking newline-delimited JSON-RPC 2.0 on stdin/stdout.
//
// The Manager owns the servers; on startup it runs the initialize handshake and
// tools/list discovery, then registers every advertised tool into the agent's
// tool registry under the "mcp" scope, qualified as `mcp__<server>__<tool>` so
// a server can never collide with or shadow a built-in tool.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ccdp/internal/execution"
	"ccdp/internal/netguard"
	"ccdp/internal/sandbox"
)

// ServerConfig describes how to launch one MCP server (config.json
// "mcp_servers": { "name": { "command": "...", "args": [...], "env": {...} } }).
// With transport "sse", BaseURL points at an SSE/Streamable-HTTP endpoint and
// command is ignored.
type ServerConfig struct {
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Transport string            `json:"transport"` // "" | "stdio" (default) | "sse"
	BaseURL   string            `json:"base_url"`  // required when transport = sse
	// NetworkAuthorized is explicit per-server consent for the host process to
	// keep this remote connection open. Each model-requested remote tool call
	// still passes the agent's per-call sandbox network capability gate.
	NetworkAuthorized bool `json:"network_authorized,omitempty"`
}

// ToolDef is the schema of one tool advertised by an MCP server (tools/list).
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// rpcMessage models one JSON-RPC 2.0 frame on the wire.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// ProtocolVersion is the MCP protocol version we advertise.
const ProtocolVersion = "2024-11-05"

// startupTimeout bounds the initialize + tools/list handshake per server.
const startupTimeout = 10 * time.Second

// maxLineLen caps one JSON-RPC frame on the wire (stdio line or SSE data
// line). Large tool results legitimately reach several MB on a single line;
// a too-small cap trips bufio.ErrTooLong and kills the whole connection with
// no recovery path, so it is deliberately generous.
const maxLineLen = 32 * 1024 * 1024

// maxMCPStderr bounds diagnostics retained from a long-lived server. Stderr
// is useful for troubleshooting but must not provide an unbounded memory sink
// for a misbehaving child process.
const maxMCPStderr = 64 * 1024

// ssePostTimeout bounds one HTTP POST to the SSE messages endpoint. The write
// helpers do not thread a context, so without this bound a hung endpoint would
// hold writeMu forever and freeze every subsequent write.
const ssePostTimeout = 120 * time.Second

// Client is one MCP server connection over stdio.
type Client struct {
	name    string
	cfg     ServerConfig
	sandbox *sandbox.Sandbox

	mu      sync.Mutex
	nextID  int
	pending map[int]chan json.RawMessage
	writeMu sync.Mutex // serializes stdin writes

	cmd        *exec.Cmd
	stdin      io.WriteCloser
	procCancel context.CancelFunc
	closed     chan struct{} // closed when the process exits or Close is called

	// closeOnce guards the closed-channel close + pending drain + onClose
	// notification so every death path (process exit, SSE stream end, explicit
	// Close) funnels through fireClosed exactly once.
	closeOnce sync.Once
	closeMu   sync.Mutex // serializes Close's kill/wait against concurrent calls
	waited    bool       // cmd.Wait already done (guarded by closeMu)
	onClose   func()     // fired once when the connection dies (Claude Code's MCP reconnect hook point)

	// SSE transport state.
	sseClient    *http.Client
	messagesURL  string
	sseTransport bool
	sseCtx       context.Context    // live while the SSE stream is open; aborted by sseCancel
	sseCancel    context.CancelFunc // cancels sseCtx (startSSE error paths and Close)

	tools []ToolDef // cached after a successful tools/list

	// onChange fires after the tool list changes at runtime
	// (notifications/tools/list_changed), so the manager can re-register.
	onChange func()

	// refreshMu coalesces a burst of tools/list_changed notifications into one
	// in-flight refresh plus at most one follow-up. A server must not be able
	// to create an unbounded goroutine storm by emitting notifications faster
	// than tools/list can complete.
	refreshMu      sync.Mutex
	refreshRunning bool
	refreshPending bool
}

// SetChangeListener registers a callback invoked when the server's tool list
// changes at runtime.
func (c *Client) SetChangeListener(fn func()) {
	c.mu.Lock()
	c.onChange = fn
	c.mu.Unlock()
}

// SetCloseListener registers a callback invoked exactly once when the
// connection dies — process exit, SSE stream end, or Close. The manager uses
// it to unregister the dead server's tools instead of leaving the model
// calling into a void (Claude Code's closeTransportAndRejectPending idea).
func (c *Client) SetCloseListener(fn func()) {
	c.mu.Lock()
	c.onClose = fn
	c.mu.Unlock()
}

// fireClosed is the single death path: drop every pending entry (waiters wake
// through their select on c.closed with a "server exited" error), close the
// lifecycle channel and notify the close listener. Idempotent via closeOnce.
//
// Pending channels are deliberately NOT closed: dispatchRaw pops a channel
// under c.mu and sends after unlocking, so closing here could race into
// "send on closed channel" and panic the whole process. Deleting the entries
// is enough — a late response finds no pending entry and is dropped.
func (c *Client) fireClosed() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		for id := range c.pending {
			delete(c.pending, id)
		}
		fn := c.onClose
		c.mu.Unlock()
		select {
		case <-c.closed:
		default:
			close(c.closed)
		}
		if fn != nil {
			fn()
		}
	})
}

// NewClient creates an unstarted client.
func NewClient(name string, cfg ServerConfig) *Client {
	return &Client{
		name:    name,
		cfg:     cfg,
		pending: map[int]chan json.RawMessage{},
		closed:  make(chan struct{}),
	}
}

// SetSandbox binds future stdio starts to the manager's current execution
// policy. A running client is not mutated: refresh creates a new generation.
func (c *Client) SetSandbox(sb *sandbox.Sandbox) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.sandbox = sb
	c.mu.Unlock()
}

// Name returns the server name.
func (c *Client) Name() string { return c.name }

// RequiresHostNetwork distinguishes a host-process HTTP connection from a
// stdio server whose command is confined by Seatbelt.
func (c *Client) RequiresHostNetwork() bool {
	return c != nil && strings.EqualFold(c.cfg.Transport, "sse")
}

// Tools returns the tools advertised by the server (empty until Start).
func (c *Client) Tools() []ToolDef {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ToolDef, len(c.tools))
	copy(out, c.tools)
	return out
}

// Start launches the server (stdio process or SSE endpoint) and performs the
// MCP handshake: initialize → notifications/initialized → tools/list.
func (c *Client) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.EqualFold(c.cfg.Transport, "sse") {
		if !c.cfg.NetworkAuthorized {
			return fmt.Errorf("mcp %s: remote server requires explicit network_authorized consent", c.name)
		}
		return c.startSSE(ctx)
	}
	c.mu.Lock()
	sb := c.sandbox
	c.mu.Unlock()
	// The caller's context bounds preparation/handshake, not the lifetime of
	// the long-lived MCP process. Manager.Close owns procCancel after startup.
	procCtx, procCancel := context.WithCancel(context.Background())
	cmd, err := execution.StartArgv(execution.StartRequest{
		Context: procCtx,
		Argv:    append([]string{c.cfg.Command}, c.cfg.Args...),
		Env:     mcpEnvironment(c.cfg.Env),
		Sandbox: sb,
	})
	if err != nil {
		procCancel()
		return fmt.Errorf("mcp %s: prepare process: %w", c.name, err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		procCancel()
		return fmt.Errorf("mcp %s: stdin: %w", c.name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		procCancel()
		return fmt.Errorf("mcp %s: stdout: %w", c.name, err)
	}
	stderr := &boundedBuffer{limit: maxMCPStderr}
	cmd.Stderr = stderr

	if sb != nil {
		sb.MarkExternalExecution()
	}
	if err := cmd.Start(); err != nil {
		procCancel()
		return fmt.Errorf("mcp %s: start %s: %w", c.name, c.cfg.Command, err)
	}
	c.cmd = cmd
	c.stdin = stdin
	c.procCancel = procCancel

	go c.readLoop(stdout)

	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	if err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "ccdp", "version": "0.2.0"},
	}); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp %s: initialize: %w%s", c.name, err, mcpDiagnosticSuffix(stderr, c.cfg))
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp %s: initialized: %w%s", c.name, err, mcpDiagnosticSuffix(stderr, c.cfg))
	}

	var list struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := c.requestJSON(ctx, "tools/list", map[string]any{}, &list); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp %s: tools/list: %w%s", c.name, err, mcpDiagnosticSuffix(stderr, c.cfg))
	}
	c.mu.Lock()
	c.tools = list.Tools
	c.mu.Unlock()
	return nil
}

// callTimeout bounds a single tools/call when the caller's context carries no
// deadline (Claude Code's MCP_TOOL_TIMEOUT, shortened for tool use).
const callTimeout = 5 * time.Minute

// Call invokes a tool on the server and returns its textual content. Image
// and resource content pieces are surfaced as placeholders so multi-modal
// output is never silently dropped.
func (c *Client) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	params := map[string]any{"name": name, "arguments": args}
	var resp struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
			URI      string `json:"uri"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	// Bound the call when the caller gave us no deadline of its own.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	if err := c.requestJSON(ctx, "tools/call", params, &resp); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, piece := range resp.Content {
		switch piece.Type {
		case "text":
			sb.WriteString(piece.Text)
			sb.WriteString("\n")
		case "image":
			fmt.Fprintf(&sb, "[image: %s, %d bytes of base64 data — the tool returned an image]\n",
				piece.MimeType, len(piece.Data))
		case "resource":
			uri := piece.URI
			if uri == "" {
				uri = "embedded resource"
			}
			fmt.Fprintf(&sb, "[resource: %s (%s)]\n", uri, piece.MimeType)
		}
	}
	out := strings.TrimRight(sb.String(), "\n")
	if resp.IsError {
		if out == "" {
			out = "tool reported an error"
		}
		return out, errors.New(out)
	}
	return out, nil
}

// refreshTools re-fetches tools/list and fires the change listener. It runs
// on notifications/tools/list_changed from the read loops.
func (c *Client) refreshTools() {
	select {
	case <-c.closed:
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	var list struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := c.requestJSON(ctx, "tools/list", map[string]any{}, &list); err != nil {
		return
	}
	c.mu.Lock()
	c.tools = list.Tools
	fn := c.onChange
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// scheduleRefresh runs one tools/list refresh at a time and coalesces any
// notifications observed while that request is in flight. The pending bit is
// deliberately a single bit: all notifications mean the same thing (the
// current list may have changed), so retaining every event has no value.
func (c *Client) scheduleRefresh() {
	if c == nil {
		return
	}
	c.refreshMu.Lock()
	if c.refreshRunning {
		c.refreshPending = true
		c.refreshMu.Unlock()
		return
	}
	c.refreshRunning = true
	c.refreshMu.Unlock()
	go func() {
		for {
			c.refreshTools()
			c.refreshMu.Lock()
			if !c.refreshPending {
				c.refreshRunning = false
				c.refreshMu.Unlock()
				return
			}
			c.refreshPending = false
			c.refreshMu.Unlock()
		}
	}()
}

// Resources lists the resources advertised by the server (resources/list).
func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var list struct {
		Resources []Resource `json:"resources"`
	}
	if err := c.requestJSON(ctx, "resources/list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	return list.Resources, nil
}

// ReadResource reads a resource by URI (resources/read). Text contents are
// returned verbatim; binary (blob) contents are surfaced as placeholders.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	var resp struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"contents"`
	}
	if err := c.requestJSON(ctx, "resources/read", map[string]any{"uri": uri}, &resp); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, pc := range resp.Contents {
		if pc.Text != "" {
			sb.WriteString(pc.Text)
			sb.WriteString("\n")
			continue
		}
		if pc.Blob != "" {
			fmt.Fprintf(&sb, "[binary resource %s (%s): %d bytes of base64 data]\n",
				pc.URI, pc.MimeType, len(pc.Blob))
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// Prompts lists the prompts advertised by the server (prompts/list).
func (c *Client) Prompts(ctx context.Context) ([]Prompt, error) {
	var list struct {
		Prompts []Prompt `json:"prompts"`
	}
	if err := c.requestJSON(ctx, "prompts/list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	return list.Prompts, nil
}

// GetPrompt retrieves a prompt's rendered messages (prompts/get) with
// template arguments.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	var resp struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	if err := c.requestJSON(ctx, "prompts/get", params, &resp); err != nil {
		return "", err
	}
	var sb strings.Builder
	if resp.Description != "" {
		fmt.Fprintf(&sb, "[%s]\n", resp.Description)
	}
	for _, m := range resp.Messages {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content.Text)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// Resource is one MCP resource (resources/list).
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// Prompt is one MCP prompt template (prompts/list).
type Prompt struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// startSSE connects to an MCP SSE/Streamable-HTTP endpoint: opens the SSE
// stream, discovers the messages endpoint, then performs the standard handshake
// over HTTP POSTs.
func (c *Client) startSSE(ctx context.Context) error {
	if c.cfg.BaseURL == "" {
		return fmt.Errorf("mcp %s: transport sse requires base_url", c.name)
	}
	c.sseTransport = true
	transport, err := netguard.OriginTransport(c.cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("mcp %s: %w", c.name, err)
	}
	c.mu.Lock()
	c.sseClient = &http.Client{Transport: transport, CheckRedirect: sameSSEOriginRedirect}
	sseClient := c.sseClient
	c.mu.Unlock()
	c.mu.Lock()
	c.sseCtx, c.sseCancel = context.WithCancel(ctx)
	c.mu.Unlock()
	// A Close racing with startup must not leave the stream running.
	select {
	case <-c.closed:
		c.sseCancel()
		return fmt.Errorf("mcp %s: sse start aborted: client closed", c.name)
	default:
	}

	sseURL := strings.TrimRight(c.cfg.BaseURL, "/")
	// A trailing /sse is the conventional endpoint; the configured URL may
	// already be the full /sse path.
	req, err := http.NewRequestWithContext(c.sseCtx, http.MethodGet, sseURL, nil)
	if err != nil {
		c.sseCancel()
		c.fireClosed()
		return fmt.Errorf("mcp %s: sse request: %w", c.name, err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := sseClient.Do(req)
	if err != nil {
		c.sseCancel()
		c.fireClosed()
		return fmt.Errorf("mcp %s: sse connect: %w", c.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		c.sseCancel()
		c.fireClosed()
		return fmt.Errorf("mcp %s: sse status %s", c.name, resp.Status)
	}

	// Read the stream until the "endpoint" event arrives (bounded wait), then
	// hand the body to the background read loop.
	messagesCh := make(chan string, 1)
	done := make(chan struct{})
	go c.sseReadLoop(resp.Body, messagesCh, done)

	select {
	case endpoint := <-messagesCh:
		messagesURL, resolveErr := resolveSSEEndpoint(c.cfg.BaseURL, endpoint)
		if resolveErr != nil {
			c.sseCancel()
			c.fireClosed()
			return fmt.Errorf("mcp %s: sse endpoint: %w", c.name, resolveErr)
		}
		c.mu.Lock()
		c.messagesURL = messagesURL
		c.mu.Unlock()
	case <-c.sseCtx.Done():
		c.sseCancel()
		c.fireClosed()
		return fmt.Errorf("mcp %s: sse stream canceled: %w", c.name, c.sseCtx.Err())
	case <-time.After(startupTimeout):
		c.sseCancel()
		c.fireClosed()
		return fmt.Errorf("mcp %s: sse endpoint event timeout", c.name)
	}
	c.mu.Lock()
	messagesURLSet := c.messagesURL != ""
	c.mu.Unlock()
	if !messagesURLSet {
		c.sseCancel()
		c.fireClosed()
		return fmt.Errorf("mcp %s: sse endpoint event missing", c.name)
	}

	return c.handshake(ctx)
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

// resolveSSEEndpoint resolves the endpoint advertised by an SSE server
// against the configured stream URL. MCP servers commonly advertise a
// relative path (for example "/messages"); passing that raw path to
// http.NewRequest would fail or accidentally target the wrong host.
func resolveSSEEndpoint(base, endpoint string) (string, error) {
	baseURL, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" || baseURL.Host == "" {
		return "", fmt.Errorf("base URL must be an http(s) URL")
	}
	endpointURL, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("invalid endpoint URL: %w", err)
	}
	resolved := baseURL.ResolveReference(endpointURL)
	if resolved.Scheme != "http" && resolved.Scheme != "https" || resolved.Host == "" {
		return "", fmt.Errorf("endpoint must resolve to an http(s) URL")
	}
	// The configured SSE origin is the trust boundary. A server-controlled
	// endpoint must not redirect the client to another host (or scheme), where
	// future transport headers/credentials could be disclosed.
	if !strings.EqualFold(resolved.Scheme, baseURL.Scheme) || !strings.EqualFold(resolved.Host, baseURL.Host) {
		return "", fmt.Errorf("endpoint crosses the configured SSE origin")
	}
	return resolved.String(), nil
}

// handshake runs initialize + initialized + tools/list once the transport is up.
func (c *Client) handshake(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	if err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "ccdp", "version": "0.2.0"},
	}); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp %s: initialize: %w", c.name, err)
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp %s: initialized: %w", c.name, err)
	}

	var list struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := c.requestJSON(ctx, "tools/list", map[string]any{}, &list); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp %s: tools/list: %w", c.name, err)
	}
	c.mu.Lock()
	c.tools = list.Tools
	c.mu.Unlock()
	return nil
}

// sseReadLoop parses the SSE event stream, dispatching message events to
// pending callers and forwarding the endpoint event.
func (c *Client) sseReadLoop(body io.ReadCloser, messagesCh chan string, done chan struct{}) {
	defer close(done)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineLen)
	var event, data strings.Builder
	flush := func() {
		e := strings.TrimSpace(event.String())
		d := strings.TrimSpace(data.String())
		event.Reset()
		data.Reset()
		switch e {
		case "endpoint":
			select {
			case messagesCh <- d:
			default:
			}
		case "message":
			c.dispatchRaw([]byte(d))
		}
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(line, "data:"))
			data.WriteString("\n")
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			log.Printf("mcp %s: sse line exceeded %d bytes; dropping connection", c.name, maxLineLen)
		} else {
			log.Printf("mcp %s: sse read: %v", c.name, err)
		}
	}

	// Stream ended (server restart): fail pending calls.
	c.fireClosed()
}

// dispatchRaw routes a JSON-RPC frame to its pending caller. Server-initiated
// notifications (no ID) and requests (ID + method) are handled here:
// tools/list_changed triggers a background re-fetch; other server requests
// get a "not supported" error reply so the server never blocks on us, and
// never leak into the client's pending-response map (IDs are independent
// counters and could collide).
func (c *Client) dispatchRaw(line []byte) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	if len(msg.ID) == 0 {
		// Server notification.
		switch msg.Method {
		case "notifications/tools/list_changed":
			c.scheduleRefresh()
		}
		return
	}
	if msg.Method != "" {
		// Server-initiated request (sampling, roots, ping…): reply with a
		// JSON-RPC error instead of silently ignoring it. The ID is echoed
		// verbatim — JSON-RPC allows string IDs and the server matches the
		// reply by exact value.
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": "method not supported by ccdp client: " + msg.Method,
			},
		})
		if err == nil {
			_ = c.write(body)
		}
		return
	}
	// Response: matched by numeric ID (our own requests always use ints). A
	// non-numeric ID can never be one of ours (e.g. a server using string
	// IDs), so drop it silently rather than guessing.
	var id int
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()
	if ok {
		select {
		case ch <- json.RawMessage(line):
		default:
		}
	}
}

// Close terminates the server process and wakes every pending caller. It is
// safe to call multiple times and concurrently with a spontaneous death: even
// when the process already exited on its own (fireClosed already ran), Close
// still closes stdin, kills and Waits so no zombie child or leaked fds remain.
func (c *Client) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	c.fireClosed() // idempotent
	c.mu.Lock()
	cancel := c.sseCancel
	procCancel := c.procCancel
	sseClient := c.sseClient
	c.procCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel() // abort the SSE stream; its read loop closes the body
	}
	if sseClient != nil {
		sseClient.CloseIdleConnections()
	}
	var err error
	intentionalStop := false
	if c.cmd != nil && !c.waited {
		// Closing stdin first unblocks any in-flight stdin.Write before the
		// kill. readLoop is deliberately not joined: Wait reaps the child and
		// closes the stdout pipe, and a final line racing with the kill may be
		// dropped — an acceptable trade-off at shutdown.
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.cmd.Process != nil {
			if killErr := c.cmd.Process.Kill(); killErr == nil {
				intentionalStop = true
			}
		}
		err = c.cmd.Wait()
		execution.CleanupStartedProcess(c.cmd)
		c.waited = true
	}
	if procCancel != nil {
		procCancel()
	}
	if err != nil && intentionalStop {
		// A process that already observed its context cancellation can report
		// exec's synthetic cancel/kill error even though it was intentionally
		// reaped successfully. A caller that needs the child's exit status can
		// inspect ProcessState before invoking Close.
		return nil
	}
	return err
}

// readLoop forwards response frames to their pending callers.
func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineLen)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Responses go to pending callers; server notifications (e.g.
		// tools/list_changed) are handled in dispatchRaw as well.
		c.dispatchRaw([]byte(line))
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			log.Printf("mcp %s: stdout line exceeded %d bytes; dropping connection", c.name, maxLineLen)
		} else {
			log.Printf("mcp %s: stdout read: %v", c.name, err)
		}
	}
	// Process exited: fail pending calls and notify the manager.
	c.fireClosed()
}

// request performs a JSON-RPC request and decodes the result or error.
func (c *Client) request(ctx context.Context, method string, params map[string]any) error {
	var out json.RawMessage
	return c.requestJSON(ctx, method, params, &out)
}

// requestJSON performs a JSON-RPC request and unmarshals the result into dst.
func (c *Client) requestJSON(ctx context.Context, method string, params map[string]any, dst any) error {
	id := c.allocID()
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.writeRequest(id, method, params); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return fmt.Errorf("mcp %s: server exited", c.name)
	case raw, ok := <-ch:
		if !ok {
			return fmt.Errorf("mcp %s: server exited", c.name)
		}
		var msg rpcMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return fmt.Errorf("mcp %s: bad response: %w", c.name, err)
		}
		if msg.Error != nil {
			return msg.Error
		}
		if dst == nil || len(msg.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(msg.Result, dst); err != nil {
			return fmt.Errorf("mcp %s: decode result: %w", c.name, err)
		}
		return nil
	}
}

// notify sends a notification (no response expected).
func (c *Client) notify(method string, params map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	return c.write(body)
}

// allocID reserves the next request id.
func (c *Client) allocID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// writeRequest marshals and writes one request.
func (c *Client) writeRequest(id int, method string, params map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	return c.write(body)
}

// write sends one newline-delimited frame on stdin (stdio) or POSTs it to the
// messages endpoint (SSE).
func (c *Client) write(body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.closed:
		return fmt.Errorf("mcp %s: server closed", c.name)
	default:
	}
	if c.sseTransport {
		return c.writeSSE(body)
	}
	buf := append(body, '\n')
	if _, err := c.stdin.Write(buf); err != nil {
		return fmt.Errorf("mcp %s: write: %w", c.name, err)
	}
	return nil
}

// writeSSE POSTs one JSON-RPC frame to the messages endpoint. The POST is
// bounded by ssePostTimeout and by the connection context (sseCtx, canceled on
// Close), so a hung server can never hold writeMu indefinitely. A
// text/event-stream body is drained in the background (it dies with the POST
// context or on Close); a plain application/json body is a synchronous
// JSON-RPC response and must be dispatched, not dropped.
func (c *Client) writeSSE(body []byte) error {
	c.mu.Lock()
	parent := c.sseCtx
	c.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	// cancel is NOT deferred: the background drain below outlives this call
	// and must keep its context alive until the body has been consumed.
	ctx, cancel := context.WithTimeout(parent, ssePostTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messagesURL, bytes.NewReader(body))
	if err != nil {
		cancel()
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.sseClient.Do(req)
	if err != nil {
		cancel()
		return fmt.Errorf("mcp %s: sse post: %w", c.name, err)
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		cancel()
		return fmt.Errorf("mcp %s: sse post status %s", c.name, resp.Status)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Streamable HTTP: the response body is itself an SSE stream feeding
		// more JSON-RPC frames; drain it in the background. The body read is
		// bounded by ctx, so the goroutine cannot leak past the POST deadline.
		go func() {
			defer cancel()
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 0, 64*1024), maxLineLen)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				if data := strings.TrimSpace(strings.TrimPrefix(line, "data:")); data != "" {
					c.dispatchRaw([]byte(data))
				}
			}
		}()
		return nil
	}
	// Plain JSON response: read it (bounded to 1MB) and route it through the
	// normal dispatcher — the body holds a single JSON-RPC response object,
	// which is exactly dispatchRaw's input contract.
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	cancel()
	if err != nil {
		return fmt.Errorf("mcp %s: sse post body: %w", c.name, err)
	}
	if raw = bytes.TrimSpace(raw); len(raw) > 0 {
		c.dispatchRaw(raw)
	}
	return nil
}

// boundedBuffer implements io.Writer for child diagnostics. It reports all
// input as consumed so an MCP process can never block on stderr after the
// retained diagnostic window fills; only the most recent limit bytes are
// kept, which preserves the useful failure tail of a noisy server.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		b.limit = maxMCPStderr
	}
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		b.truncated = true
		return len(p), nil
	}
	if remaining := b.limit - len(b.buf); len(p) > remaining {
		drop := len(p) - remaining
		b.buf = append(b.buf[drop:], p...)
		b.truncated = true
	} else {
		b.buf = append(b.buf, p...)
	}
	if len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := string(b.buf)
	if b.truncated {
		out += "\n[…MCP stderr truncated…]"
	}
	return out
}

var (
	mcpURLUserInfoPattern = regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`)
	mcpBearerPattern      = regexp.MustCompile(`(?i)(\bbearer\s+)[^\s,;]+`)
)

// mcpDiagnosticSuffix returns a bounded, redacted stderr suffix suitable for
// a startup/handshake error. Diagnostics are deliberately not logged from the
// background read loop: MCP servers may print credentials or private URLs.
func mcpDiagnosticSuffix(stderr *boundedBuffer, cfg ServerConfig) string {
	if stderr == nil {
		return ""
	}
	diagnostic := strings.TrimSpace(stderr.String())
	if diagnostic == "" {
		return ""
	}
	// Configured MCP environment is an explicit capability, but its values are
	// still secrets from the model's point of view. Redact every non-empty
	// configured value before returning diagnostics rather than trying to infer
	// whether a custom variable name is sensitive.
	for _, value := range cfg.Env {
		if value != "" {
			diagnostic = strings.ReplaceAll(diagnostic, value, "<redacted>")
		}
	}
	diagnostic = mcpURLUserInfoPattern.ReplaceAllString(diagnostic, `${1}<redacted>@`)
	diagnostic = mcpBearerPattern.ReplaceAllString(diagnostic, `${1}<redacted>`)
	const diagnosticLimit = 4 * 1024
	if len(diagnostic) > diagnosticLimit {
		diagnostic = "…" + diagnostic[len(diagnostic)-diagnosticLimit:]
	}
	return " (stderr: " + diagnostic + ")"
}

// mcpEnvironment removes inherited provider credentials while preserving
// explicit server-local environment from config. Explicit MCP env is a
// deliberate capability of that server, not an accidental leak from the
// host process.
func mcpEnvironment(extra map[string]string) []string {
	values := map[string]string{}
	for _, entry := range execution.SanitizedEnvironmentFor(execution.EnvironmentMCP, os.Environ()) {
		key, value := entry, ""
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key, value = entry[:i], entry[i+1:]
		}
		values[key] = value
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}
