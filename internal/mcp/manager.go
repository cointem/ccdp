package mcp

import (
	"context"
	"fmt"
	"log"
	"sync"

	"ccdp/internal/tools"
)

// Manager owns all configured MCP server connections and exposes their tools to
// the agent's tool registry under the "mcp" scope.
type Manager struct {
	mu       sync.Mutex
	clients  map[string]*Client // ordered by registration via the servers map
	names    []string
	registry *tools.Registry // set by RegisterTools for hot refresh
}

// NewManager builds an empty manager.
func NewManager() *Manager {
	return &Manager{clients: map[string]*Client{}}
}

// Start launches every configured server and runs the MCP handshake. Servers
// that fail to start are skipped with a log line so one broken server never
// blocks the agent; the manager still serves the healthy ones.
func (m *Manager) Start(ctx context.Context, servers map[string]ServerConfig) {
	for name, cfg := range servers {
		if cfg.Command == "" {
			log.Printf("mcp: server %q has no command; skipping", name)
			continue
		}
		c := NewClient(name, cfg)
		if err := c.Start(ctx); err != nil {
			log.Printf("mcp: %s", err)
			continue
		}
		m.mu.Lock()
		m.clients[name] = c
		m.names = append(m.names, name)
		m.mu.Unlock()
	}
}

// Names returns the connected server names.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.names))
	copy(out, m.names)
	return out
}

// Server returns a connected client by name.
func (m *Manager) Server(name string) (*Client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[name]
	return c, ok
}

// ToolNames returns server name → advertised tool names for every connected
// server (used by the /mcp TUI command).
func (m *Manager) ToolNames() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	for _, name := range m.names {
		for _, t := range m.clients[name].Tools() {
			out[name] = append(out[name], t.Name)
		}
	}
	return out
}

// RegisterTools wraps every advertised tool as a tools.Tool and registers it
// into the registry under the "mcp" scope. On name collisions between servers,
// the first registered server wins. The registry reference is kept so a
// server's notifications/tools/list_changed can re-sync at runtime, and so a
// dead server's tools are unregistered instead of lingering (a call into a
// dead server would just fail with "server exited" forever).
func (m *Manager) RegisterTools(reg *tools.Registry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registry = reg
	for _, name := range m.names {
		client := m.clients[name]
		m.registerClientTools(client)
		resync := func() {
			reg.UnregisterScope("mcp")
			m.mu.Lock()
			for _, n := range m.names {
				m.registerClientTools(m.clients[n])
			}
			m.mu.Unlock()
		}
		// Hot refresh: when the server advertises a changed tool list,
		// re-register everything under the "mcp" scope (old entries are
		// replaced by name; removed tools are unregistered first).
		client.SetChangeListener(resync)
		// Connection death (process exit, SSE stream end): drop the server
		// and re-sync the "mcp" scope from the survivors.
		client.SetCloseListener(func() {
			log.Printf("mcp: server %q disconnected; unregistering its tools", name)
			m.mu.Lock()
			delete(m.clients, name)
			for i, n := range m.names {
				if n == name {
					m.names = append(m.names[:i], m.names[i+1:]...)
					break
				}
			}
			m.mu.Unlock()
			resync()
		})
	}
}

// registerClientTools registers one client's current tools (caller holds mu).
func (m *Manager) registerClientTools(client *Client) {
	if m.registry == nil {
		return
	}
	for _, def := range client.Tools() {
		if def.Name == "" {
			continue
		}
		m.registry.RegisterIn("mcp", &mcpTool{client: client, def: def})
	}
}

// Close shuts down every server. The clients are closed WITHOUT the manager
// lock held: each Close fires the close listener, which re-enters the manager
// to unregister the server (and would deadlock if m.mu were already held).
func (m *Manager) Close() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = map[string]*Client{}
	m.names = nil
	m.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
}

// mcpTool adapts an MCP tool definition to the tools.Tool interface.
type mcpTool struct {
	client *Client
	def    ToolDef
}

func (t *mcpTool) Name() string { return t.def.Name }

func (t *mcpTool) Description() string {
	d := t.def.Description
	if d == "" {
		d = fmt.Sprintf("MCP tool %q provided by server %q", t.def.Name, t.client.Name())
	}
	return d
}

func (t *mcpTool) Parameters() map[string]any {
	if t.def.InputSchema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return t.def.InputSchema
}

func (t *mcpTool) Run(ctx *tools.Context) (string, error) {
	return t.client.Call(ctx, t.def.Name, ctx.Args)
}
