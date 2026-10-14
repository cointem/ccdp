package mcp

import (
	"context"
	"fmt"
	"io"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"ccdp/internal/execution"
)

func (c *Client) startStdio(ctx context.Context) error {
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
	started := false
	defer func() {
		if !started {
			execution.CleanupStartedProcess(cmd)
		}
	}()

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
	cmd.SetStderr(stderr)

	if sb != nil {
		sb.MarkExternalExecution()
	}
	if err := cmd.Start(); err != nil {
		procCancel()
		return fmt.Errorf("mcp %s: start %s: %w", c.name, c.cfg.Command, err)
	}
	c.cmd = cmd
	started = true
	c.stdin = stdin
	c.procCancel = procCancel

	if err := c.connectStdio(ctx, stdout, stdin); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp %s: connect: %w%s", c.name, err, mcpDiagnosticSuffix(stderr, c.cfg))
	}
	return nil
}

func (c *Client) connectStdio(ctx context.Context, reader io.ReadCloser, writer io.WriteCloser) error {
	lifetime, cancel := context.WithCancel(context.Background())
	transport := &guardedIOTransport{IOTransport: sdk.IOTransport{Reader: reader, Writer: checkedWriter{writer}, MaxLineLength: maxLineLen}}
	c.mu.Lock()
	c.sessionCancel, c.sessionCtx = cancel, lifetime
	c.mu.Unlock()
	stopStartup := context.AfterFunc(ctx, func() { cancel(); _ = reader.Close(); _ = writer.Close() })
	defer stopStartup()
	if c.isClosed() {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return fmt.Errorf("client closed")
	}
	session, err := c.sdkClient().Connect(ctx, transport, &sdk.ClientSessionOptions{ProtocolVersion: ProtocolVersion})
	if err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return err
	}
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	go func() { _ = session.Wait(); c.fireClosed() }()
	var list struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := c.requestJSON(ctx, "tools/list", nil, &list); err != nil {
		_ = c.Close()
		return err
	}
	c.mu.Lock()
	c.tools = list.Tools
	c.mu.Unlock()
	if !stopStartup() || ctx.Err() != nil || c.isClosed() {
		_ = c.Close()
		return fmt.Errorf("startup cancelled or connection closed")
	}
	return nil
}
