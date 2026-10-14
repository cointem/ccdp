package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"ccdp/internal/execution"
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

// ProtocolVersion is the MCP protocol version we advertise.
const ProtocolVersion = "2024-11-05"

// startupTimeout bounds transport setup and the entire handshake per server.
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

// stdioWriteTimeout caps a frame send even when its caller has no deadline.
const stdioWriteTimeout = 120 * time.Second

// Client is one MCP server connection over stdio or HTTP.
type Client struct {
	name    string
	cfg     ServerConfig
	sandbox *sandbox.Sandbox

	mu sync.Mutex

	cmd        *execution.Process
	stdin      io.WriteCloser
	procCancel context.CancelFunc
	closed     chan struct{} // closed when the process exits or Close is called

	// closeOnce guards the closed channel. Listener delivery
	// is separately guarded by mu so a late subscriber observes the same death.
	closeOnce     sync.Once
	closeNotified bool       // close listener delivered; guarded by mu
	closeMu       sync.Mutex // serializes Close's kill/wait against concurrent calls
	waited        bool       // cmd.Wait already done (guarded by closeMu)
	onClose       func()     // fired once when the connection dies (Claude Code's MCP reconnect hook point)

	// The SDK owns protocol/session state for both transports; ccdp owns lifetime and policy.
	sseClient     *http.Client
	sessionCancel context.CancelFunc
	session       *sdk.ClientSession
	sessionCtx    context.Context

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
	c.notifyClosed()
}

// fireClosed publishes transport death once. The SDK owns request retirement.
func (c *Client) fireClosed() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		close(c.closed)
		c.mu.Unlock()
	})
	// A listener can be installed after a transport died during preparation.
	// Invoke outside closeOnce too: manager cleanup may re-enter Client.Close.
	c.notifyClosed()
}

func (c *Client) notifyClosed() {
	c.mu.Lock()
	var fn func()
	select {
	case <-c.closed:
		if !c.closeNotified && c.onClose != nil {
			c.closeNotified = true
			fn = c.onClose
		}
	default:
	}
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// NewClient creates an unstarted client.
func NewClient(name string, cfg ServerConfig) *Client {
	return &Client{
		name:   name,
		cfg:    cfg,
		closed: make(chan struct{}),
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
	ctx, cancelStartup := context.WithTimeout(ctx, startupTimeout)
	defer cancelStartup()
	if strings.EqualFold(c.cfg.Transport, "sse") {
		if !c.cfg.NetworkAuthorized {
			return fmt.Errorf("mcp %s: remote server requires explicit network_authorized consent", c.name)
		}
		return c.startSSE(ctx)
	}
	return c.startStdio(ctx)
}

// callTimeout bounds a single tools/call when the caller's context carries no
// deadline (Claude Code's MCP_TOOL_TIMEOUT, shortened for tool use).
const callTimeout = 5 * time.Minute

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

// Close terminates the server process and wakes every pending caller. It is
// safe to call multiple times and concurrently with a spontaneous death: even
// when the process already exited on its own (fireClosed already ran), Close
// still closes stdin, kills and Waits so no zombie child or leaked fds remain.
func (c *Client) Close() error {
	c.fireClosed() // callbacks may re-enter Close; never hold closeMu here
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	c.mu.Lock()
	cancel := c.sessionCancel
	procCancel := c.procCancel
	sseClient := c.sseClient
	session := c.session

	c.procCancel = nil
	c.mu.Unlock()
	// SDK Close waits for outstanding calls. Cancel our request lifetime first
	// so a peer with a hanging response cannot prevent graceful SDK shutdown.
	if cancel != nil {
		cancel()
	}
	if session != nil {
		_ = session.Close()
	}
	if sseClient != nil {
		sseClient.CloseIdleConnections()
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	var err error
	if c.cmd != nil && !c.waited {
		// Closing stdin first unblocks any in-flight stdin.Write before the
		// kill. Stop joins the process and closes the remaining pipe handles.
		err = c.cmd.Stop("cancelled")
		c.waited = err == nil
	}
	if procCancel != nil {
		procCancel()
	}
	return err
}
