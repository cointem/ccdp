package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/tools"
)

// Manager owns all configured MCP server connections and exposes their tools to
// the agent's tool registry under the "mcp" scope.
type Manager struct {
	mu         sync.Mutex
	clients    map[string]*Client // ordered by registration via the servers map
	names      []string
	registry   *tools.Registry // set by RegisterTools for hot refresh
	generation uint64
	refs       map[uint64]int
	retired    map[uint64][]*Client
	closed     bool
	sandbox    *sandbox.Sandbox
	registryMu sync.Mutex // serializes complete catalog publications
	// refreshMu serializes candidate lifetimes with Close. A settings reload
	// may have to start an external server before its durable settings fact is
	// committed; keeping this lock until Commit/Abort prevents shutdown from
	// observing the gap and makes the post-commit publication non-fallible.
	refreshMu sync.Mutex
}

// NewManager builds an empty manager.
func NewManager() *Manager {
	return &Manager{clients: map[string]*Client{}, refs: map[uint64]int{}, retired: map[uint64][]*Client{}}
}

// SetSandbox changes the policy used by subsequently prepared/reloaded MCP
// stdio clients. Existing clients belong to their generation and are not
// mutated mid-request.
func (m *Manager) SetSandbox(sb *sandbox.Sandbox) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sandbox = sb
	m.mu.Unlock()
}

func (m *Manager) sandboxSnapshot() *sandbox.Sandbox {
	m.mu.Lock()
	sb := m.sandbox
	m.mu.Unlock()
	return sb
}

// Start launches every configured server and runs the MCP handshake. Servers
// that fail to start are skipped with a log line so one broken server never
// blocks the agent; the manager still serves the healthy ones.
func (m *Manager) Start(ctx context.Context, servers map[string]ServerConfig) {
	if m == nil {
		return
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	// Start is retained for compatibility with callers that historically treat
	// one broken optional server as a warning. New runtime construction uses
	// StartChecked so a candidate configuration can fail atomically.
	for _, name := range sortedServerNames(servers) {
		cfg := servers[name]
		if !strings.EqualFold(cfg.Transport, "sse") && cfg.Command == "" {
			log.Printf("mcp: server %q has no command; skipping", name)
			continue
		}
		c := NewClient(name, cfg)
		c.SetSandbox(m.sandboxSnapshot())
		if err := c.Start(ctx); err != nil {
			log.Printf("mcp: %s", err)
			continue
		}
		m.registryMu.Lock()
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			m.registryMu.Unlock()
			_ = c.Close()
			return
		}
		m.clients[name] = c
		m.names = append(m.names, name)
		if m.generation == 0 {
			m.generation = 1
		}
		generation := m.generation
		m.replaceRegistryLocked()
		m.mu.Unlock()
		m.registryMu.Unlock()
		m.configureClient(c, name, generation)
	}
}

func sortedServerNames(servers map[string]ServerConfig) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// StartChecked prepares every server before publishing any of them. A
// handshake failure closes all candidates and leaves the manager unchanged.
// This is the construction/reload path; callers can safely retry with a new
// candidate without leaking a partially started MCP set.
func (m *Manager) StartChecked(ctx context.Context, servers map[string]ServerConfig) error {
	if m == nil {
		return fmt.Errorf("mcp: nil manager")
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	type candidate struct {
		name   string
		client *Client
	}
	candidates := make([]candidate, 0, len(servers))
	cleanup := func() {
		for _, c := range candidates {
			_ = c.client.Close()
		}
	}
	for _, name := range sortedServerNames(servers) {
		cfg := servers[name]
		if !strings.EqualFold(cfg.Transport, "sse") && cfg.Command == "" {
			cleanup()
			return fmt.Errorf("mcp: server %q has no command", name)
		}
		client := NewClient(name, cfg)
		client.SetSandbox(m.sandboxSnapshot())
		if err := client.Start(ctx); err != nil {
			cleanup()
			return err
		}
		candidates = append(candidates, candidate{name: name, client: client})
	}
	m.registryMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.registryMu.Unlock()
		cleanup()
		return fmt.Errorf("mcp: manager is closed")
	}
	if m.generation == 0 {
		m.generation = 1
	}
	generation := m.generation
	for _, c := range candidates {
		m.clients[c.name] = c.client
		m.names = append(m.names, c.name)
	}
	m.replaceRegistryLocked()
	m.mu.Unlock()
	m.registryMu.Unlock()
	for _, c := range candidates {
		m.configureClient(c.client, c.name, generation)
	}
	return nil
}

// Refresh atomically swaps the complete configured server set. Existing MCP
// clients are retired by generation and remain alive while any step lease
// references that generation; a new step sees only the new tool bindings.
func (m *Manager) Refresh(ctx context.Context, servers map[string]ServerConfig) error {
	return m.refresh(ctx, servers, nil, false)
}

// RefreshWithSandbox is the transactional reload variant. The supplied
// sandbox is used only by candidate clients; the manager's current policy is
// untouched if startup fails.
func (m *Manager) RefreshWithSandbox(ctx context.Context, servers map[string]ServerConfig, sb *sandbox.Sandbox) error {
	return m.refresh(ctx, servers, sb, true)
}

func (m *Manager) refresh(ctx context.Context, servers map[string]ServerConfig, candidateSandbox *sandbox.Sandbox, overrideSandbox bool) error {
	candidate, err := m.PrepareRefresh(ctx, servers, candidateSandbox, overrideSandbox)
	if err != nil {
		return err
	}
	return candidate.Commit()
}

type refreshClient struct {
	name   string
	client *Client
}

// RefreshCandidate owns MCP clients that have completed their handshake but
// are not yet visible through Manager or the tool registry. The owner must
// call exactly one of Commit or Abort. This narrow two-phase boundary lets an
// Agent persist the corresponding SettingsChanged fact before exposing a new
// executable extension generation.
type RefreshCandidate struct {
	manager   *Manager
	clients   []refreshClient
	mu        sync.Mutex
	finished  bool
	committed bool
}

// PrepareRefresh starts and handshakes every configured server without
// changing the manager's current clients, generation, or registry scope.
// Failed preparation closes every candidate client and leaves the old
// generation untouched. The candidate keeps Manager.refreshMu until Commit or
// Abort so Close cannot race the durable-before-publication boundary.
func (m *Manager) PrepareRefresh(ctx context.Context, servers map[string]ServerConfig, candidateSandbox *sandbox.Sandbox, overrideSandbox bool) (*RefreshCandidate, error) {
	if m == nil {
		return nil, fmt.Errorf("mcp: nil manager")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.refreshMu.Lock()
	cleanup := func(candidates []refreshClient) {
		for _, c := range candidates {
			if c.client != nil {
				_ = c.client.Close()
			}
		}
	}
	// Check before launching the first process. The lock also prevents Close
	// from changing this state until the candidate resolves.
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		m.refreshMu.Unlock()
		return nil, fmt.Errorf("mcp: manager is closed")
	}
	candidates := make([]refreshClient, 0, len(servers))
	for _, name := range sortedServerNames(servers) {
		cfg := servers[name]
		if !strings.EqualFold(cfg.Transport, "sse") && cfg.Command == "" {
			cleanup(candidates)
			m.refreshMu.Unlock()
			return nil, fmt.Errorf("mcp: server %q has no command", name)
		}
		client := NewClient(name, cfg)
		if overrideSandbox {
			client.SetSandbox(candidateSandbox)
		} else {
			client.SetSandbox(m.sandboxSnapshot())
		}
		if err := client.Start(ctx); err != nil {
			cleanup(candidates)
			_ = client.Close()
			m.refreshMu.Unlock()
			return nil, err
		}
		candidates = append(candidates, refreshClient{name: name, client: client})
	}
	return &RefreshCandidate{manager: m, clients: candidates}, nil
}

// Commit publishes a prepared candidate as one manager generation and one
// registry catalog update. Once preparation succeeded, the manager lock makes
// this path non-fallible except for a concurrent shutdown, which is serialized
// by refreshMu and therefore cannot occur until Commit returns.
func (c *RefreshCandidate) Commit() error {
	if c == nil || c.manager == nil {
		return fmt.Errorf("mcp: nil refresh candidate")
	}
	c.mu.Lock()
	if c.finished {
		committed := c.committed
		c.mu.Unlock()
		if committed {
			return nil
		}
		return fmt.Errorf("mcp: refresh candidate already aborted")
	}
	c.finished = true
	c.committed = true
	manager := c.manager
	clients := append([]refreshClient(nil), c.clients...)
	c.clients = nil
	c.mu.Unlock()

	manager.registryMu.Lock()
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		manager.registryMu.Unlock()
		for _, item := range clients {
			_ = item.client.Close()
		}
		manager.refreshMu.Unlock()
		return fmt.Errorf("mcp: manager is closed")
	}
	oldGen := manager.generation
	if oldGen == 0 {
		oldGen = 1
	}
	oldClients := make([]*Client, 0, len(manager.clients))
	for _, client := range manager.clients {
		oldClients = append(oldClients, client)
	}
	manager.generation = oldGen + 1
	newGen := manager.generation
	manager.clients = make(map[string]*Client, len(clients))
	manager.names = make([]string, 0, len(clients))
	for _, item := range clients {
		manager.clients[item.name] = item.client
		manager.names = append(manager.names, item.name)
	}
	if len(oldClients) > 0 {
		manager.retired[oldGen] = oldClients
	}
	// AcquireStep takes registryMu then manager.mu, exactly this order, so a
	// step can never pair a new catalog with an old generation reference.
	manager.replaceRegistryLocked()
	manager.mu.Unlock()
	manager.registryMu.Unlock()
	for _, item := range clients {
		manager.configureClient(item.client, item.name, newGen)
	}
	manager.releaseGeneration(oldGen)
	manager.refreshMu.Unlock()
	return nil
}

// Abort closes only the uncommitted candidate clients and leaves the active
// generation and registry untouched. It is safe to call after a failed
// durable settings admission.
func (c *RefreshCandidate) Abort() error {
	if c == nil || c.manager == nil {
		return nil
	}
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return nil
	}
	c.finished = true
	clients := append([]refreshClient(nil), c.clients...)
	c.clients = nil
	manager := c.manager
	c.mu.Unlock()
	var closeErr error
	for _, item := range clients {
		if err := item.client.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("mcp %s close: %w", item.name, err))
		}
	}
	manager.refreshMu.Unlock()
	return closeErr
}

func (m *Manager) configureClient(client *Client, name string, generation uint64) {
	if client == nil {
		return
	}
	client.SetChangeListener(func() { m.syncRegistry() })
	client.SetCloseListener(func() { m.clientClosed(name, client, generation) })
}

func (m *Manager) clientClosed(name string, client *Client, generation uint64) {
	m.registryMu.Lock()
	m.mu.Lock()
	current, active := m.clients[name]
	if active && current == client {
		delete(m.clients, name)
		for i, n := range m.names {
			if n == name {
				m.names = append(m.names[:i], m.names[i+1:]...)
				break
			}
		}
		m.replaceRegistryLocked()
	}
	m.mu.Unlock()
	m.registryMu.Unlock()
	if active && current == client {
		log.Printf("mcp: server %q disconnected; unregistering its tools", name)
	}
	if !active || current != client {
		m.releaseGeneration(generation)
	}
}

// BeginLease pins the current MCP generation until the returned release
// function is called. Agent step boundaries own this release operation.
func (m *Manager) BeginLease() func() {
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return func() {}
	}
	generation := m.generation
	if generation == 0 {
		generation = 1
		m.generation = generation
	}
	m.refs[generation]++
	m.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { m.releaseGenerationRef(generation) }) }
}

// StepLease combines the registry catalog view with the MCP client generation
// that owns its implementations. It is the only supported step boundary for
// agent execution; acquiring the two pieces independently can otherwise race
// a refresh between the catalog and generation locks.
type StepLease struct {
	Tools   *tools.Lease
	release func()
	retain  func() func()
	mu      sync.Mutex
	closed  bool
}

func (m *Manager) AcquireStep(reg *tools.Registry) *StepLease {
	if m == nil {
		if reg == nil {
			reg = tools.NewRegistry()
		}
		return &StepLease{Tools: reg.Acquire()}
	}
	m.registryMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.registryMu.Unlock()
		if reg == nil {
			reg = tools.NewRegistry()
		}
		return &StepLease{Tools: reg.Acquire()}
	}
	generation := m.generation
	if generation == 0 {
		generation = 1
		m.generation = generation
	}
	var lease *tools.Lease
	if reg != nil {
		lease = reg.Acquire()
	} else {
		lease = tools.NewRegistry().Acquire()
	}
	m.refs[generation]++
	m.mu.Unlock()
	m.registryMu.Unlock()
	return &StepLease{Tools: lease, release: func() { m.releaseGenerationRef(generation) }, retain: func() func() {
		m.mu.Lock()
		m.refs[generation]++
		m.mu.Unlock()
		return func() { m.releaseGenerationRef(generation) }
	}}
}

// Retain pins the exact generation captured by this lease, even after reload.
func (l *StepLease) Retain() *StepLease {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	copy := &StepLease{Tools: l.Tools, retain: l.retain}
	if l.retain != nil {
		copy.release = l.retain()
	}
	return copy
}

func (l *StepLease) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	l.mu.Unlock()
	if l.release != nil {
		l.release()
	}
	if l.Tools != nil {
		l.Tools.Close()
	}
}

func (m *Manager) releaseGenerationRef(generation uint64) {
	m.mu.Lock()
	if m.refs[generation] > 0 {
		m.refs[generation]--
	}
	shouldClose := m.refs[generation] == 0
	if shouldClose {
		delete(m.refs, generation)
	}
	clients := append([]*Client(nil), m.retired[generation]...)
	if shouldClose {
		delete(m.retired, generation)
	}
	m.mu.Unlock()
	if shouldClose {
		for _, c := range clients {
			_ = c.Close()
		}
	}
}

func (m *Manager) releaseGeneration(generation uint64) {
	m.mu.Lock()
	if m.refs[generation] != 0 {
		m.mu.Unlock()
		return
	}
	clients := append([]*Client(nil), m.retired[generation]...)
	delete(m.retired, generation)
	m.mu.Unlock()
	for _, c := range clients {
		_ = c.Close()
	}
}

func (m *Manager) syncRegistry() {
	if m == nil {
		return
	}
	m.registryMu.Lock()
	defer m.registryMu.Unlock()
	m.mu.Lock()
	reg := m.registry
	m.replaceRegistryLocked()
	m.mu.Unlock()
	_ = reg
}

// replaceRegistryLocked is called while m.mu is held. Registry.ReplaceScope
// makes one complete catalog publication instead of exposing an empty scope.
func (m *Manager) replaceRegistryLocked() {
	if m.registry == nil {
		return
	}
	values := make([]tools.Tool, 0)
	for _, name := range m.names {
		client := m.clients[name]
		for _, def := range client.Tools() {
			if def.Name != "" {
				values = append(values, &mcpTool{client: client, def: def})
			}
		}
	}
	m.registry.ReplaceScope("mcp", values)
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

// RequiresHostNetwork reports whether the qualified tool is backed by a
// remote HTTP MCP server. Local stdio MCP remains governed by its Seatbelt
// process policy and does not consume the host-network capability.
func (m *Manager) RequiresHostNetwork(toolName string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	m.mu.Unlock()
	for _, client := range clients {
		if !client.RequiresHostNetwork() {
			continue
		}
		for _, def := range client.Tools() {
			if protocol.MCPToolName(client.Name(), def.Name) == toolName {
				return true
			}
		}
	}
	return false
}

// RegisterTools wraps every advertised tool as a tools.Tool and registers it
// into the registry under the "mcp" scope. Each tool is published under its
// qualified name (`mcp__<server>__<tool>`), so a server can never shadow a
// built-in tool or another server's tool by advertising a colliding raw name.
// The registry reference is kept so a server's notifications/tools/list_changed
// can re-sync at runtime, and so a dead server's tools are unregistered instead
// of lingering (a call into a dead server would just fail with "server exited"
// forever).
func (m *Manager) RegisterTools(reg *tools.Registry) {
	if reg == nil {
		return
	}
	m.registryMu.Lock()
	m.mu.Lock()
	m.registry = reg
	clients := make([]struct {
		name   string
		client *Client
	}, 0, len(m.names))
	generation := m.generation
	for _, name := range m.names {
		clients = append(clients, struct {
			name   string
			client *Client
		}{name, m.clients[name]})
	}
	m.replaceRegistryLocked()
	m.mu.Unlock()
	m.registryMu.Unlock()
	for _, item := range clients {
		m.configureClient(item.client, item.name, generation)
	}
	// replaceRegistryLocked above already published the complete view.
}

// Close shuts down every server. The clients are closed WITHOUT the manager
// lock held: each Close fires the close listener, which re-enters the manager
// to unregister the server (and would deadlock if m.mu were already held).
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.registryMu.Lock()
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	for generation, retired := range m.retired {
		_ = generation
		clients = append(clients, retired...)
	}
	m.clients = map[string]*Client{}
	m.names = nil
	m.retired = map[uint64][]*Client{}
	m.refs = map[uint64]int{}
	m.closed = true
	reg := m.registry
	m.registry = nil
	m.mu.Unlock()
	m.registryMu.Unlock()
	if reg != nil {
		reg.UnregisterScope("mcp")
	}
	seen := map[*Client]struct{}{}
	var closeErr error
	for _, c := range clients {
		if c != nil {
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = struct{}{}
			if err := c.Close(); err != nil {
				closeErr = errors.Join(closeErr, err)
			}
		}
	}
	return closeErr
}

// mcpTool adapts an MCP tool definition to the tools.Tool interface.
type mcpTool struct {
	client *Client
	def    ToolDef
}

// Name returns the qualified registry identity (`mcp__<server>__<tool>`), never
// the raw server-supplied name. Qualifying the name is what prevents a server
// from shadowing a built-in tool or another server's tool by advertising a
// colliding name; the raw name (t.def.Name) is still what goes back on the wire.
func (t *mcpTool) Name() string { return protocol.MCPToolName(t.client.Name(), t.def.Name) }

func (t *mcpTool) Description() string {
	d := t.def.Description
	if d == "" {
		d = fmt.Sprintf("MCP tool %q provided by server %q", t.def.Name, t.client.Name())
	}
	return d
}

func (t *mcpTool) Parameters() map[string]any {
	if t.def.InputSchema == nil {
		return tools.WithCapabilityRequest(map[string]any{"type": "object", "properties": map[string]any{}})
	}
	return tools.WithCapabilityRequest(t.def.InputSchema)
}

func (t *mcpTool) Run(ctx *tools.Context) (string, error) {
	if t.client.RequiresHostNetwork() && !ctx.HostNetworkAllowed {
		return "", fmt.Errorf("remote MCP tool %s requires per-call outbound network authorization", t.Name())
	}
	return t.client.Call(ctx, t.def.Name, ctx.Args)
}
