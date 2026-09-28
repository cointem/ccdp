package tools

import (
	"ccdp/internal/sandbox"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRegressionGrepFIFO(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { readGrepFile(&Context{Context: ctx, Sandbox: sandbox.New(root)}, path); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	// Release the blocked read so the diagnostic does not leak a goroutine.
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader did not unblock")
	}
	t.Fatal("Grep opened FIFO in blocking mode instead of rejecting it")
}
