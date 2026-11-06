package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/plugin"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"ccdp/internal/session/archive"
)

func TestSettledChildInputRetryDoesNotStartContinuation(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("memory=%v/cancelled=%v", memory, cancelled), func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				provider := &childRuntimeTestProvider{name: "child-input-retry", stream: func(ctx context.Context, _ llm.CompletionRequest, _ func(string)) (llm.StreamResult, error) {
					if calls.Add(1) == 1 {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return llm.StreamResult{}, ctx.Err()
						}
					}
					return llm.StreamResult{Text: "answer", FinishReason: "stop"}, nil
				}}
				cfg := isolatedTestConfig(t, provider.Name())
				cfg.NoSessionPersistence = memory
				models := plugin.NewModelRegistry()
				models.Register(provider)
				models.Route(cfg.Model, provider.Name())
				a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true, ProviderRegistry: models})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(a.Close)
				r, err := a.services.supervisor.launch(a, a.Context(), childTask{Description: "initial work"}, childPurposeTask, "input-retry")
				if err != nil {
					t.Fatal(err)
				}
				<-entered
				r.mu.Lock()
				row := r.fact.Child
				r.mu.Unlock()
				reader, err := a.Sessions().OpenReader(context.Background(), row.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				cmd := protocol.NewSubmitInput("followup-command", row.SessionID, "followup-input", "followup work", protocol.InputFollowup)
				cmd.ExpectedRunID = row.Run.ID
				cmd.Input.Images = []protocol.InputImage{inputPNG(t, 50)}
				receipt, err := reader.Submit(context.Background(), cmd)
				if err != nil || receipt.Rejected() {
					t.Fatalf("queue: %+v %v", receipt, err)
				}
				if cancelled {
					_, err = reader.Submit(context.Background(), protocol.Command{ID: "interrupt", SessionID: row.SessionID, ExpectedRunID: row.Run.ID, Type: protocol.CommandInterrupt})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					close(release)
				}
				waitManaged(t, r)
				wantCalls := int32(2)
				if cancelled {
					wantCalls = 1
				}
				if calls.Load() != wantCalls {
					t.Fatalf("initial turns=%d, want %d", calls.Load(), wantCalls)
				}
				assertRetry := func(a *Agent, reader protocol.SessionClient, cmd protocol.Command) {
					t.Helper()
					before := len(a.services.supervisor.directory.Runs())
					receipt, err := reader.Submit(context.Background(), cmd)
					if err != nil || receipt.CommandID != cmd.ID {
						t.Fatalf("retry: %+v %v", receipt, err)
					}
					if cancelled {
						if receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidState || receipt.Error.Message == "" {
							t.Fatalf("cancelled input lost terminal rejection: %+v", receipt)
						}
					} else if receipt.Status != protocol.ReceiptApplied {
						t.Fatalf("delivered input lost applied receipt: %+v", receipt)
					}
					if len(a.services.supervisor.directory.Runs()) != before {
						t.Fatal("old input created a new run")
					}
				}
				assertRetry(a, reader, cmd)
				// A fresh identity still starts work. The old input must also stay
				// terminal after this continuation replaces the current run.
				next := protocol.NewSubmitInput("new-command", row.SessionID, "new-input", "new work", protocol.InputFollowup)
				receipt, err = reader.Submit(context.Background(), next)
				if err != nil || receipt.Rejected() {
					t.Fatalf("fresh continuation: %+v %v", receipt, err)
				}
				current, err := a.services.supervisor.lookup(row.SessionID)
				if err != nil || current == r {
					t.Fatalf("missing continuation: %v", err)
				}
				waitManaged(t, current)
				if calls.Load() != wantCalls+1 {
					t.Fatalf("continuation calls=%d", calls.Load())
				}
				assertRetry(a, reader, cmd)
				again := cmd
				again.ID = "same-input-new-envelope"
				assertRetry(a, reader, again)
				var reopened *Agent
				if memory {
					reopened, err = a.OpenSession(a.SessionID())
				} else {
					if err := a.CloseContext(context.Background()); err != nil {
						t.Fatal(err)
					}
					var snapshot *archive.Snapshot
					snapshot, err = archive.Load(cfg.SessionDir, a.SessionID())
					if err == nil {
						reopened, err = Resume(&cfg, snapshot)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(reopened.Close)
				restoredReader, err := reopened.Sessions().OpenReader(context.Background(), row.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				assertRetry(reopened, restoredReader, cmd)
				for _, change := range []string{"text", "strategy", "image"} {
					conflict := cmd
					input := *cmd.Input
					conflict.ID = protocol.CommandID("conflict-" + change)
					conflict.Input = &input
					switch change {
					case "text":
						input.Text += " changed"
					case "strategy":
						input.Strategy = protocol.InputSteer
					case "image":
						input.Images = []protocol.InputImage{inputPNG(t, 51)}
					}
					receipt, err := restoredReader.Submit(context.Background(), conflict)
					if err != nil || receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidCommand {
						t.Fatalf("%s conflict: %+v %v", change, receipt, err)
					}
				}
			})
		}
	}
}

func TestStoppedChildQueuedInputRequiresFreshIdentity(t *testing.T) {
	for _, strategy := range []protocol.InputStrategy{protocol.InputFollowup, protocol.InputMessage} {
		t.Run(string(strategy), func(t *testing.T) {
			a, _ := sessionRepairAgent(t, true)
			p, err := openSessionPersistence(a.services.cfg, "queued-child", a.conversation.createdAt)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			cmd := protocol.NewSubmitInput("retry-command", "queued-child", "queued-input", "work", strategy)
			digest, err := submitInputDigest(*cmd.Input)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.PersistInputQueuedWithDigest("queued-input", "work", string(strategy), "turn-1", a.conversation.createdAt, digest); err != nil {
				t.Fatal(err)
			}
			rows, blobs, err := p.ReadSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			close(done)
			r := &managedRun{done: done, fact: session.ChildRunRecorded{Child: protocol.ChildSession{SessionID: "queued-child", Run: protocol.RunView{ID: "queued-run", Status: "unknown"}}}, memory: rows}
			r.memoryBlobs, err = copyMemoryArtifacts(rows, blobs)
			if err != nil {
				t.Fatal(err)
			}
			a.services.supervisor.directory.Restore(r.fact.Child, r, true)
			reader, err := a.Sessions().OpenReader(context.Background(), "queued-child")
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := reader.Submit(context.Background(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			if strategy == protocol.InputMessage {
				if receipt.Status != protocol.ReceiptScheduled {
					t.Fatalf("undelivered mailbox message lost its queued receipt: %+v", receipt)
				}
			} else if receipt.Error == nil || receipt.Error.Code != protocol.ErrorInvalidState {
				t.Fatalf("abandoned input was silently accepted: %+v", receipt)
			}
			if len(a.services.supervisor.directory.Runs()) != 1 {
				t.Fatal("queued input retry created a run")
			}
		})
	}
}
