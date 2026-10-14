package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func TestExtraReviewForkPreservesImageBlobs(t *testing.T) {
	cfg := testPersistenceConfig(t)
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	img := inputPNG(t, 255)
	msg := messages.Message{Role: messages.RoleUser, Content: "inspect ![image](local.png)", ImagesFrozen: true, ImageAttachments: []messages.ImageAttachment{{Path: "local.png", MediaType: img.MediaType, Data: img.Data}}}
	if err = a.appendHistory(msg); err != nil {
		t.Fatal(err)
	}
	id, err := a.Fork(-1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := session.OpenArtifactStoreReadOnly(filepath.Join(cfg.SessionDir, id))
	found := false
	for _, m := range snapshot.History {
		for _, im := range m.ImageAttachments {
			found = true
			data, err := artifacts.Read(session.BlobRef{Hash: im.BlobHash, Size: im.BlobSize, MediaType: im.MediaType}, maxImageBytes)
			if err != nil || !bytes.Equal(data, img.Data) {
				t.Errorf("fork contains image ref without usable bytes: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("fork dropped image metadata")
	}
}

func TestExtraReviewForkLongHistory(t *testing.T) {
	cfg := testPersistenceConfig(t)
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 130; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: fmt.Sprintf("request %d", i)}, messages.Message{Role: messages.RoleAssistant, Content: fmt.Sprintf("answer %d", i)}); err != nil {
			t.Fatalf("source history %d: %v", i, err)
		}
	}
	if _, err := a.Fork(-1); err != nil {
		t.Errorf("cannot fork ordinary 260-message conversation: %v", err)
	}
}

func TestExtraReviewChildForkBlobs(t *testing.T) {
	p := &childRuntimeTestProvider{name: "fork-image-provider", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	img := inputPNG(t, 255)
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "inspect ![image](local.png)", ImagesFrozen: true, ImageAttachments: []messages.ImageAttachment{{Path: "local.png", MediaType: img.MediaType, Data: img.Data}}}); err != nil {
		t.Fatal(err)
	}
	r, err := a.services.supervisor.launch(a, context.Background(), childTask{Description: "inspect inherited screenshot", ContextMode: "fork"}, childPurposeTask, "images")
	if err != nil {
		t.Fatal(err)
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		t.Errorf("forked child cannot read parent's images: %v", r.err)
	}
}

func TestExtraReviewChildForkLongHistory(t *testing.T) {
	p := &childRuntimeTestProvider{name: "fork-long-provider", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
	}}
	a, _ := newChildRuntimeTestAgent(t, p)
	for i := 0; i < 130; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: fmt.Sprintf("request %d", i)}, messages.Message{Role: messages.RoleAssistant, Content: fmt.Sprintf("answer %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := a.services.supervisor.launch(a, context.Background(), childTask{Description: "inspect inherited conversation", ContextMode: "fork"}, childPurposeTask, "long")
	if err != nil {
		t.Fatal(err)
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		t.Errorf("forked child cannot load long history: %v", r.err)
	}
}

func TestReviewManualCheckpointAttributesLaterRecordedWrites(t *testing.T) {
	a, root := codingFixture(t)
	ctx := context.Background()
	p1, err := a.captureRecoveryPoint(ctx, "manual-1", "first")
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(name, value string) {
		t.Helper()
		path := filepath.Join(root, name)
		finish, err := a.recordCodingWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
		if err = finish(); err != nil {
			t.Fatal(err)
		}
	}
	mutate("chosen.txt", "first known change\n")
	if _, err = a.captureRecoveryPoint(ctx, "manual-2", "second"); err != nil {
		t.Fatal(err)
	}
	mutate("staged.txt", "second known change\n")
	if _, err = a.captureRecoveryPoint(ctx, "manual-3", "third"); err != nil {
		t.Fatal(err)
	}
	mutate("chosen.txt", "third known change\n")
	current, err := a.captureCoding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := a.inverseMutationPlan(p1, current, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range plan.Edits {
		if edit.Conflict != "" {
			t.Errorf("fully recorded edit is marked uncertain: %s: %s", edit.Path, edit.Conflict)
		}
	}
	if len(plan.Edits) != 2 {
		t.Fatalf("restore edits: %+v", plan.Edits)
	}
	if _, err := a.codingState().store.Apply(ctx, plan, a.services.sandbox.Snapshot()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"chosen.txt", "staged.txt"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(data) != "base\n" {
			t.Fatalf("restore %s: %q %v", path, data, err)
		}
	}
}

func TestReviewRound3ChildInteractionWakesParent(t *testing.T) {
	for _, kind := range []string{"question", "plan"} {
		t.Run(kind, func(t *testing.T) {
			root := newRuntimeAgent(t)
			child := newRuntimeAgent(t)
			child.mu.Lock()
			if kind == "question" {
				child.decisions.pendingQuestion = &protocol.QuestionRequest{ID: "question1", Questions: clarificationQuestions()}
			} else {
				child.plan.pendingPlan = &PlanRequest{ID: "plan1", Plan: "implement feature"}
			}
			child.mu.Unlock()
			s := root.services.supervisor
			r := &managedRun{agent: child, parent: root, fact: session.ChildRunRecorded{Child: protocol.ChildSession{
				SessionID: protocol.SessionID(child.conversation.sessionID), ParentSessionID: protocol.SessionID(root.conversation.sessionID), Purpose: childPurposeTask,
				Run: protocol.RunView{ID: "run1", Status: "running", WaitPolicy: "notify"},
			}}}
			s.directory.Restore(r.fact.Child, r, true)
			defer func() {
				s.directory.Forget(protocol.SessionID(child.conversation.sessionID))
			}()
			rows, err := s.ListChildren(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("wrong child fixture: %+v", rows)
			}
			if rows[0].Run.Status == "running" {
				t.Errorf("child blocked on %s still advertised as running", kind)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			got, err := root.waitAgentEvent(ctx)
			if err != nil || got != "attention" {
				t.Errorf("parent did not learn child %s needs input: got=%q err=%v", kind, got, err)
			}
		})
	}
}

func TestHistoryInitializationByteLimitAndFailurePublication(t *testing.T) {
	cfg := testPersistenceConfig(t)
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Each message fits a normal turn transaction; their combined initializer exceeds 8 MiB.
	for i := 0; i < 3; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: strings.Repeat("x", 3<<20)}); err != nil {
			t.Fatal(err)
		}
	}
	id, err := a.Fork(-1)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSession(cfg.SessionDir, id)
	if err != nil || len(loaded.History) < 3 {
		t.Fatalf("byte-batched fork: %v", err)
	}
	before, err := session.ListSessions(cfg.SessionDir)
	if err != nil {
		t.Fatal(err)
	}
	// Bypass ordinary turn admission to exercise rejection of a single oversized message.
	a.mu.Lock()
	a.conversation.durable.History = []messages.Message{{Role: messages.RoleUser, Content: strings.Repeat("x", 9<<20)}}
	a.mu.Unlock()
	if _, err := a.Fork(-1); err == nil {
		t.Fatal("oversized history message accepted")
	}
	after, err := session.ListSessions(cfg.SessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed initialization published a session: before=%d after=%d", len(before), len(after))
	}
	entries, _ := os.ReadDir(cfg.SessionDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".history-init-") {
			t.Fatal("staging directory leaked")
		}
	}
}

func TestForkChildResourcesUsePublishedSession(t *testing.T) {
	p := &childRuntimeTestProvider{name: "published-fork-provider", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "done", FinishReason: "stop"}, nil
	}}
	parent, _ := newChildRuntimeTestAgent(t, p)
	cfg, opts, err := childOptionsFromParent(parent, childPurposeTask)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SessionID = newSessionID()
	child, err := newForkAgent(&cfg, opts, parent.persistenceHandle(), []messages.Message{{Role: messages.RoleUser, Content: "copied context"}})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	target := filepath.Join(cfg.SessionDir, cfg.SessionID)
	if child.services.baseCfg.SessionDir != cfg.SessionDir || child.services.resources.SessionDir() != target {
		t.Fatalf("runtime retains staging paths: base=%s resources=%s", child.services.baseCfg.SessionDir, child.services.resources.SessionDir())
	}
	output, failed := child.executeTool(messages.ToolCall{ID: "write-published", Name: "Write", Arguments: map[string]any{"file_path": "published.txt", "content": "published state"}})
	if failed {
		t.Fatalf("write after publication: %s", output)
	}
	if data, err := os.ReadFile(filepath.Join(cfg.Workspace, "published.txt")); err != nil || string(data) != "published state" {
		t.Fatalf("write: %s %v", data, err)
	}
	blobStore, err := child.persistenceHandle().RequestArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := blobStore.PutContext(context.Background(), []byte("saved tool output"))
	if err != nil {
		t.Fatal(err)
	}
	artifacts := session.OpenArtifactStoreReadOnly(target)
	if _, err := artifacts.Read(ref, 100); err != nil {
		t.Fatal(err)
	}
	submitChildRuntimeTestInput(t, child, "published-first", "continue")
	waitChildRuntimeTestIdle(t, child)
	submitChildRuntimeTestInput(t, child, "published-second", "continue again")
	deadline := time.Now().Add(8 * time.Second)
	for {
		view, err := child.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !view.Busy && view.Usage.TurnCount >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("continuation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	entries, _ := os.ReadDir(cfg.SessionDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".history-init-") {
			t.Fatal("runtime recreated staging directory")
		}
	}
}

func TestCancelledHistoryInitializationDoesNotPublish(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.SessionID = newSessionID()
	ctx, cancel := context.WithCancel(context.Background())
	staged, publish, cleanup, err := stageHistorySession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	p, err := openSessionPersistence(&staged, cfg.SessionID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cancel()
	if err := p.InitializeHistory(ctx, []messages.Message{{Role: messages.RoleUser, Content: "cancelled"}}, "fork"); err != context.Canceled {
		t.Fatalf("initialize: %v", err)
	}
	if err := publish(); err != context.Canceled {
		t.Fatalf("publish: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.SessionDir, cfg.SessionID)); !os.IsNotExist(err) {
		t.Fatalf("cancelled session became visible: %v", err)
	}
}
