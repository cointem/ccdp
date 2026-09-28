package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
)

func TestProcessCompletionIsSharedAndClosesSignalEntry(t *testing.T) {
	for i := 0; i < 20; i++ {
		p := newProcess(exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0"))
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		var readers sync.WaitGroup
		for n := 0; n < 4; n++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				if err := p.Wait(); err != nil {
					t.Error(err)
				}
			}()
		}
		readers.Wait()
		if err := p.signal(syscall.SIGKILL, "cancelled"); !errors.Is(err, os.ErrProcessDone) {
			t.Fatal("signalling remained possible after reap", err)
		}
		if err := p.Stop("cancelled"); err != nil || p.Reason() != "" {
			t.Fatal("stop changed completed result", p.Reason(), err)
		}
	}
}

func TestAbandonedProcessClosesPreparedPipes(t *testing.T) {
	p := newProcess(exec.CommandContext(context.Background(), "/bin/sh", "-c", "cat"))
	if _, err := p.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	CleanupStartedProcess(p)
	for _, f := range []*os.File{p.stdinRead, p.input.(*os.File), p.stdoutRead, p.stdoutWrite} {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("pipe leaked", err)
		}
	}
}
