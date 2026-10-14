package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"ccdp/internal/protocol"
)

func TestSettingsDrainKeepsOwnerResponsiveAndPreservesQueuedRevision(t *testing.T) {
	a := newRuntimeAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	var enteredOnce sync.Once
	a.mu.Lock()
	a.capabilities.capabilityGrants["test"] = protocol.CapabilityRequest{Kind: "network"}
	a.turn.turnCancel = func() { enteredOnce.Do(func() { close(entered) }) }
	a.lifecycle.turnWG.Add(1)
	a.mu.Unlock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(a.lifecycle.turnWG.Done) }
	defer release()
	sid := protocol.SessionID(a.SessionID())
	revoked := make(chan protocol.Receipt, 1)
	go func() {
		r, _ := a.Submit(ctx, protocol.Command{ID: "drain", SessionID: sid, Type: protocol.CommandRevokeCapabilities, RevokeCapabilities: &protocol.RevokeCapabilities{All: true}})
		revoked <- r
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("drain did not start")
	}
	view, _ := a.Snapshot(ctx)
	effort := "high"
	queued := make(chan protocol.Receipt, 1)
	go func() {
		r, _ := a.Submit(ctx, protocol.Command{ID: "after-drain", SessionID: sid, ExpectedRevision: view.Revision.LogSeq, Type: protocol.CommandSetGeneration, Generation: &protocol.SetGeneration{ReasoningEffort: &effort}})
		queued <- r
	}()
	receipt, err := a.Submit(ctx, protocol.Command{ID: "interrupt-drain", SessionID: sid, Type: protocol.CommandInterrupt})
	if err != nil || receipt.Rejected() {
		t.Fatalf("owner blocked during resource drain: %+v %v", receipt, err)
	}
	select {
	case <-revoked:
		t.Fatal("revocation completed before worker exit")
	default:
	}
	release()
	select {
	case r := <-revoked:
		if r.Rejected() {
			t.Fatalf("revocation: %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("revocation did not settle")
	}
	select {
	case r := <-queued:
		if !r.Rejected() || r.Error == nil || r.Error.Code != protocol.ErrorStaleRevision {
			t.Fatalf("queued settings rebased their expected revision: %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("queued settings lost")
	}
}
