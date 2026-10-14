package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

type signalPhaseClient struct {
	headlessTestClient
	phase   string
	ready   chan struct{}
	once    sync.Once
	watches atomic.Int32
}

func (c *signalPhaseClient) entering() { c.once.Do(func() { close(c.ready) }) }
func (c *signalPhaseClient) Watch(ctx context.Context, cursor protocol.Cursor) (protocol.Subscription, error) {
	count := c.watches.Add(1)
	if c.phase == "watch" || c.phase == "resync" && count == 2 {
		c.entering()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c.phase == "snapshot" {
		c.entering()
		return &headlessTestSubscription{ch: make(chan protocol.Update)}, nil
	}
	return c.headlessTestClient.Watch(ctx, cursor)
}
func (c *signalPhaseClient) Submit(ctx context.Context, cmd protocol.Command) (protocol.Receipt, error) {
	if c.phase == "decision" && cmd.Type == protocol.CommandApproveTool || c.phase == "submit" && cmd.Type == protocol.CommandSubmitInput {
		c.entering()
		<-ctx.Done()
		return protocol.Receipt{}, ctx.Err()
	}
	if cmd.Type == protocol.CommandSubmitInput {
		if c.phase == "consume" {
			c.entering()
			return protocol.Receipt{CommandID: cmd.ID, Status: protocol.ReceiptScheduled}, nil
		}
		if c.phase == "resync" {
			c.publish(protocol.Update{Type: protocol.UpdateResyncRequired})
			return protocol.Receipt{CommandID: cmd.ID, Status: protocol.ReceiptScheduled}, nil
		}
	}
	return c.headlessTestClient.Submit(ctx, cmd)
}

// Use a subprocess so real signals cannot interfere with other package tests.
func TestHeadlessSignalExitAcrossLifecycle(t *testing.T) {
	if phase := os.Getenv("CCDP_TEST_SIGNAL_PHASE"); phase != "" {
		value, _ := strconv.Atoi(os.Getenv("CCDP_TEST_SIGNAL"))
		c := &signalPhaseClient{phase: phase, ready: make(chan struct{})}
		c.snapshot = headlessTestSnapshot()
		if phase == "decision" {
			c.snapshot.Approval = &protocol.ApprovalView{ID: "pending"}
		}
		go func() {
			<-c.ready
			time.Sleep(10 * time.Millisecond)
			_ = syscall.Kill(os.Getpid(), syscall.Signal(value))
		}()
		os.Exit(runHeadlessClient(c, []string{"hello"}, outJSON, 10))
	}
	self, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for _, phase := range []string{"watch", "snapshot", "decision", "submit", "consume", "resync"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(phase+"/"+sig.String(), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, self, "-test.run=^TestHeadlessSignalExitAcrossLifecycle$")
				cmd.Env = append(os.Environ(), "CCDP_TEST_SIGNAL_PHASE="+phase, "CCDP_TEST_SIGNAL="+strconv.Itoa(int(sig)))
				out, e := cmd.CombinedOutput()
				exit, ok := e.(*exec.ExitError)
				if !ok || exit.ExitCode() != 130 {
					t.Fatalf("%s interrupted by %s: error=%v output=%s", phase, sig, e, out)
				}
			})
		}
	}
}

func TestHeadlessErrorCodeDistinguishesClientCancellation(t *testing.T) {
	if got := headlessErrorCode(context.Background(), context.Canceled); got != 1 {
		t.Fatalf("client cancellation=%d", got)
	}
	if got := headlessErrorCode(context.Background(), context.DeadlineExceeded); got != 2 {
		t.Fatalf("startup deadline=%d", got)
	}
}
