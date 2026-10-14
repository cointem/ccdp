package mcp

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type observedPipe struct {
	*io.PipeWriter
	once    sync.Once
	entered chan struct{}
}

func (w *observedPipe) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	return w.PipeWriter.Write(p)
}
func receiveError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("request did not settle")
		return nil
	}
}

func TestStdioWriteAndQueueCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	pipe := &observedPipe{PipeWriter: writer, entered: make(chan struct{})}
	idleReader, idleWriter := io.Pipe()
	defer idleWriter.Close()
	transport := &guardedIOTransport{IOTransport: sdk.IOTransport{Reader: idleReader, Writer: checkedWriter{pipe}}}
	c, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	message, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan error, 1)
	go func() { first <- c.Write(firstCtx, message) }()
	select {
	case <-pipe.entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	// Queue cancellation must not tear down the connection owned by another call.
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- c.Write(secondCtx, message) }()
	cancelSecond()
	if err := receiveError(t, second); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued error: %v", err)
	}
	cancelFirst()
	if err := receiveError(t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("write error: %v", err)
	}
	if err := c.Write(context.Background(), message); err == nil {
		t.Fatal("closed transport accepted another frame")
	}
}

type shortPipe struct{ closed bool }

func (w *shortPipe) Write(p []byte) (int, error) { return len(p) / 2, nil }
func (w *shortPipe) Close() error                { w.closed = true; return nil }

func TestStdioShortFrameRetiresConnection(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	output := &shortPipe{}
	transport := &guardedIOTransport{IOTransport: sdk.IOTransport{Reader: reader, Writer: checkedWriter{output}}}
	conn, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	message, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Write(context.Background(), message); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error: %v", err)
	}
	if !output.closed {
		t.Fatal("partial frame left connection open")
	}
}
