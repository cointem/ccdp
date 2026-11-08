package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRegressionRgFIFO(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	toolCtx := scopedTestContext(t, root)
	toolCtx.Context = ctx
	done := make(chan error, 1)
	go func() { _, err := readRgFileDetailed(toolCtx, path, toolCtx.readLimit()); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("expected FIFO rejection, got: %v", err)
		}
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
	t.Fatal("Rg opened FIFO in blocking mode instead of rejecting it")
}
