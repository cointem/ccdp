package mcp

import (
	"context"
	"io"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The SDK handles framing and dispatch. This wrapper adds cancellation-aware
// write admission: cancelling a queued writer preserves the active frame;
// cancelling an active write retires the connection because a partial frame
// cannot be safely followed by another JSON-RPC message.
type guardedIOTransport struct{ sdk.IOTransport }

func (t *guardedIOTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := t.IOTransport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &guardedIOConnection{Connection: conn, gate: make(chan struct{}, 1)}, nil
}

type guardedIOConnection struct {
	sdk.Connection
	gate chan struct{}
}

func (c *guardedIOConnection) Write(parent context.Context, message jsonrpc.Message) error {
	ctx, cancel := context.WithTimeout(parent, stdioWriteTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.gate <- struct{}{}:
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.Connection.Close(); close(stopped) })
	err := c.Connection.Write(ctx, message)
	if !stop() {
		<-stopped
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		_ = c.Connection.Close()
	}
	return err
}

// IOTransport currently ignores short successful writes; a pipe adapter must
// surface them as fatal frame errors instead of silently dropping a suffix.
type checkedWriter struct{ io.WriteCloser }

func (w checkedWriter) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
