package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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

func TestRunSignalsCannotReachHostProcess(t *testing.T) {
	// This fixture is outside the workload sandbox. Never target a live user process.
	host := exec.Command("/bin/sleep", "30")
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Process.Kill(); _ = host.Wait() }()
	req := testRequest(t, Request{Context: context.Background(), Timeout: 5 * time.Second})
	for _, signal := range []string{"0", "TERM", "KILL"} {
		req.Command = fmt.Sprintf("kill -%s %d", signal, host.Process.Pid)
		res, err := Run(req)
		if err != nil || res.TimedOut || res.ExitCode == 0 || !strings.Contains(res.Output, "Operation not permitted") {
			t.Fatalf("host signal %s: %+v %v", signal, res, err)
		}
	}
}

func TestRunCommandTimeoutCanSignalChild(t *testing.T) {
	path, err := exec.LookPath("timeout")
	if err != nil {
		t.Skip("timeout is not installed")
	}
	res, err := Run(testRequest(t, Request{Context: context.Background(),
		Command: QuoteArg(path) + " 0.2 /bin/sleep 10", Timeout: 5 * time.Second}))
	if err != nil || res.TimedOut || res.ExitCode != 124 {
		t.Fatalf("timeout could not terminate its child: %+v %v", res, err)
	}
}
