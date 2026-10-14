package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func sessionRepairAgent(t *testing.T, memory bool) (*Agent, config.Config) {
	t.Helper()
	cfg := testPersistenceConfig(t)
	cfg.NoSessionPersistence = memory
	cfg.EnableGuardian, cfg.EnableMemory = config.BoolPtr(false), config.BoolPtr(false)
	cfg.MCPServers = nil
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a, cfg
}

func sessionRepairSeed(t *testing.T, a *Agent, history []messages.Message) {
	t.Helper()
	if err := a.persistenceHandle().InitializeHistory(context.Background(), history, "repair-seed"); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.conversation.durable.History = messages.Clone(history)
	a.mu.Unlock()
}

func sessionRepairHistory(n int) []messages.Message {
	history := make([]messages.Message, n)
	for i := range history {
		history[i] = messages.Message{ID: fmt.Sprintf("repair-message-%d", i), Role: messages.RoleAssistant,
			Content: fmt.Sprintf("answer %d", i), CreatedAt: time.Unix(int64(i+1), 0).UTC()}
	}
	return history
}

func TestSessionRepairLongHistoryRestoreAndRemove(t *testing.T) {
	a, root := codingFixture(t)
	want := sessionRepairHistory(300)
	sessionRepairSeed(t, a, want)
	point, err := a.captureRecoveryPoint(context.Background(), "repair-checkpoint", "long conversation")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.appendHistory(messages.Message{ID: "repair-later", Role: messages.RoleAssistant, Content: "later answer"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "chosen.txt")
	finish, err := a.beforeCodingWrite(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	out, err := a.runRestore(context.Background(), protocol.Command{ID: "repair-prepare", Workflow: &protocol.WorkflowCommand{Action: "prepare", ID: point.ID, Mode: "both"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.runRestore(context.Background(), protocol.Command{ID: "repair-apply", Workflow: &protocol.WorkflowCommand{Action: "apply", ID: deliveryID(t, out)}}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "base\n" {
		t.Fatalf("restore files: %q, %v", got, err)
	}
	assertHistory := func() {
		t.Helper()
		if got := a.History(); !reflect.DeepEqual(got, want) {
			t.Fatalf("live history differs: got %d messages, want %d", len(got), len(want))
		}
		loaded, err := LoadSession(a.services.cfg.SessionDir, a.SessionID())
		if err != nil || !reflect.DeepEqual(loaded.History, want) {
			t.Fatalf("replay history differs: %v", err)
		}
		view, err := a.Snapshot(context.Background())
		if err != nil || len(view.Transcript) == 0 || view.Transcript[len(view.Transcript)-1].ID != want[len(want)-1].ID {
			t.Fatalf("transcript did not follow replacement: %v", err)
		}
	}
	assertHistory()
	if err := a.appendHistory(messages.Message{ID: "repair-remove", Role: messages.RoleAssistant, Content: "remove this answer"}); err != nil {
		t.Fatal(err)
	}
	if err := a.removeMessagesCommand(1, "repair-remove-command"); err != nil {
		t.Fatal(err)
	}
	assertHistory()
}

func TestSessionRepairOversizedRestoreLeavesFilesUntouched(t *testing.T) {
	a, root := codingFixture(t)
	history := sessionRepairHistory(2)
	for i := range history {
		history[i].Content = strings.Repeat("x", int(session.DefaultMaxTransactionBytes/2))
	}
	sessionRepairSeed(t, a, history)
	point, err := a.captureRecoveryPoint(context.Background(), "repair-oversized", "oversized checkpoint history")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.clearHistoryCommand("repair-clear"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "chosen.txt")
	finish, err := a.beforeCodingWrite(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("keep these newer bytes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	out, err := a.runRestore(context.Background(), protocol.Command{ID: "repair-oversized-prepare", Workflow: &protocol.WorkflowCommand{Action: "prepare", ID: point.ID, Mode: "both"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runRestore(context.Background(), protocol.Command{ID: "repair-oversized-apply", Workflow: &protocol.WorkflowCommand{Action: "apply", ID: deliveryID(t, out)}})
	if !errors.Is(err, session.ErrTooLarge) {
		t.Fatalf("oversized replacement should fail admission: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "keep these newer bytes\n" {
		t.Fatalf("failed admission modified files: %q, %v", got, err)
	}
	if len(a.History()) != 0 {
		t.Fatal("failed admission published replacement history")
	}
}

func TestSessionRepairReplacementPreflightUsesRealTransactionSize(t *testing.T) {
	a, _ := sessionRepairAgent(t, true)
	if err := a.appendHistory(messages.Message{ID: "repair-before-limit", Role: messages.RoleAssistant, Content: "old context"}); err != nil {
		t.Fatal(err)
	}
	history := sessionRepairHistory(2)
	for i := range history {
		history[i].Content = strings.Repeat("x", int(session.DefaultMaxTransactionBytes/2))
	}
	p := a.persistenceHandle()
	const turn = "repair-limit-turn"
	err := p.ValidateHistoryReplacement(history, turn)
	if !errors.Is(err, session.ErrTooLarge) {
		t.Fatalf("expected sizing probe to exceed transaction limit: %v", err)
	}
	// Size the replacement exactly to the admission boundary. The same event
	// and transaction envelope must then fit when the real writer commits it.
	message := err.Error()
	start := strings.Index(message, "transaction is ")
	var size, limit int64
	if start < 0 {
		t.Fatal(err)
	}
	if _, err := fmt.Sscanf(message[start:], "transaction is %d bytes, limit %d", &size, &limit); err != nil {
		t.Fatal(err)
	}
	history[1].Content = history[1].Content[:len(history[1].Content)-int(size-limit)]
	if err := p.ValidateHistoryReplacement(history, turn); err != nil {
		t.Fatalf("sized replacement did not pass admission: %v", err)
	}
	if err := p.PersistHistoryMessages(history, turn); err != nil {
		t.Fatalf("admitted replacement failed its real commit: %v", err)
	}
}

func sessionRepairAssertImage(t *testing.T, a *Agent, message messages.Message, image []byte) {
	t.Helper()
	if !message.ImagesFrozen || len(message.ImageAttachments) != 1 {
		t.Fatalf("image metadata lost: frozen=%v attachments=%d", message.ImagesFrozen, len(message.ImageAttachments))
	}
	got, err := a.frozenImageBytes(message.ImageAttachments[0])
	if err != nil || !bytes.Equal(got, image) {
		t.Fatalf("frozen image bytes changed: %v", err)
	}
	parts, _, found, err := a.imageContentPartsForMessage(a.services.cfg.Workspace, message, maxAttachmentBytes)
	if err != nil || !found {
		t.Fatalf("cannot construct provider image: %v", err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
	for _, part := range parts {
		if part.ImageURL != nil && part.ImageURL.URL == want {
			return
		}
	}
	t.Fatal("provider content omitted exact frozen image bytes")
}

func TestSessionRepairDiskToMemoryPreservesHistoryAndPendingImages(t *testing.T) {
	a, cfg := sessionRepairAgent(t, false)
	historical, pending := inputPNG(t, 12), inputPNG(t, 34)
	message := messages.Message{ID: "repair-historical-image", Role: messages.RoleUser, Content: "historical ![image](deleted.png)",
		ImagesFrozen: true, ImageAttachments: []messages.ImageAttachment{{Path: "deleted.png", MediaType: historical.MediaType, Data: historical.Data}}}
	if err := a.appendHistory(message); err != nil {
		t.Fatal(err)
	}
	cmd := protocol.NewSubmitInput("repair-pending-command", protocol.SessionID(a.SessionID()), "repair-pending-image", "pending image", protocol.InputFollowup)
	cmd.Input.Images = []protocol.InputImage{pending}
	a.mu.Lock()
	a.turn.busy = true
	a.mu.Unlock()
	receipt := a.applySubmitInput(cmd)
	a.mu.Lock()
	a.turn.busy = false
	a.mu.Unlock()
	if receipt.Rejected() {
		t.Fatal(receipt.Error)
	}
	id := a.SessionID()
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NoSessionPersistence = true
	resumed, err := Resume(&cfg, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resumed.Close)
	// The imported session owns its blob bytes; neither source log nor original
	// attachment paths remain available for subsequent provider construction.
	if err := os.RemoveAll(filepath.Join(cfg.SessionDir, id)); err != nil {
		t.Fatal(err)
	}
	history := resumed.History()
	if len(history) != 1 || history[0].ID != message.ID {
		t.Fatal("historical message identity lost")
	}
	sessionRepairAssertImage(t, resumed, history[0], historical.Data)
	input, ok, err := resumed.claimPendingInputAndAppend("turn-repair", protocol.InputFollowup)
	if err != nil || !ok || input.ID != "repair-pending-image" {
		t.Fatalf("pending input identity lost: %+v, %v", input, err)
	}
	history = resumed.History()
	sessionRepairAssertImage(t, resumed, history[len(history)-1], pending.Data)
}

func TestSessionRepairSyntheticMemoryResumePreservesImages(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.NoSessionPersistence = true
	historical, pending := inputPNG(t, 56), inputPNG(t, 78)
	snapshot := &SessionSnapshot{ID: "repair-synthetic", Workspace: cfg.Workspace, Model: cfg.Model,
		History: []messages.Message{{ID: "repair-synthetic-history", Role: messages.RoleUser, Content: "history ![image](history.png)", ImagesFrozen: true,
			ImageAttachments: []messages.ImageAttachment{{MediaType: historical.MediaType, Data: historical.Data}}}},
		PendingInputs:      []protocol.InputView{{ID: "repair-synthetic-pending", Text: "pending ![image](pending.png)", Strategy: protocol.InputFollowup}},
		PendingAttachments: map[string][]messages.ImageAttachment{"repair-synthetic-pending": {{MediaType: pending.MediaType, Data: pending.Data}}}}
	a, err := Resume(&cfg, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	sessionRepairAssertImage(t, a, a.History()[0], historical.Data)
	_, ok, err := a.claimPendingInputAndAppend("turn-repair-synthetic", protocol.InputFollowup)
	if err != nil || !ok {
		t.Fatalf("cannot deliver synthetic pending image: %v", err)
	}
	history := a.History()
	sessionRepairAssertImage(t, a, history[len(history)-1], pending.Data)
}

func TestSessionRepairMemoryForkSurvivesSourceCloseAndSessionSwitch(t *testing.T) {
	a, cfg := sessionRepairAgent(t, true)
	image := inputPNG(t, 90)
	if err := a.appendHistory(messages.Message{ID: "repair-fork-image", Role: messages.RoleUser, Content: "fork ![image](fork.png)", ImagesFrozen: true,
		ImageAttachments: []messages.ImageAttachment{{MediaType: image.MediaType, Data: image.Data}}}); err != nil {
		t.Fatal(err)
	}
	sourceID := a.SessionID()
	forkID, err := a.Fork(-1)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := a.OpenSession(forkID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fork.Close)
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	sessionRepairAssertImage(t, fork, fork.History()[0], image.Data)
	if err := fork.appendHistory(messages.Message{ID: "repair-fork-answer", Role: messages.RoleAssistant, Content: "fork answer"}); err != nil {
		t.Fatal(err)
	}
	original, err := fork.OpenSession(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(original.Close)
	if err := fork.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if original.SessionID() != sourceID || len(original.History()) != 1 {
		t.Fatal("opening original session merged fork history")
	}
	again, err := original.OpenSession(forkID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	if got := again.History(); len(got) != 2 || got[1].ID != "repair-fork-answer" {
		t.Fatal("switching back lost fork's later history")
	}
	sessionRepairAssertImage(t, again, again.History()[0], image.Data)
	if _, err := os.Stat(cfg.SessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("memory session operations touched disk: %v", err)
	}
}

func TestSessionRepairReplacementOutputRemainsAddressable(t *testing.T) {
	a, _ := sessionRepairAgent(t, false)
	if err := a.appendHistory(messages.Message{ID: "repair-old", Role: messages.RoleAssistant, Content: "old context"}); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("replacement summary\n", 16000)
	candidate := []messages.Message{{ID: "repair-replacement", Role: messages.RoleAssistant, Content: text}}
	if err := a.persistenceHandle().PersistHistoryMessages(candidate, "turn-repair-replacement"); err != nil {
		t.Fatal(err)
	}
	page, err := a.Sessions().ReadTranscript(context.Background(), protocol.SessionID(a.SessionID()), 0, 64)
	if err != nil || len(page.Items) == 0 || page.Items[len(page.Items)-1].ID != candidate[0].ID {
		t.Fatalf("replacement transcript item missing: %v", err)
	}
	item := page.Items[len(page.Items)-1]
	output, err := a.Sessions().ReadOutput(context.Background(), protocol.SessionID(a.SessionID()), item.ID, 0, 65536)
	if err != nil || output.Text != text[:65536] || !output.More {
		t.Fatalf("replacement transcript item cannot read full output: %v", err)
	}
}

func TestSessionRepairReplacementDiscardsOldToolRecovery(t *testing.T) {
	a, _ := sessionRepairAgent(t, false)
	call := messages.ToolCall{ID: "repair-uncertain-call", Name: "Read", Arguments: map[string]any{"file_path": "x"}}
	if err := a.appendHistory(messages.AssistantWithTools("old tool round", []messages.ToolCall{call})); err != nil {
		t.Fatal(err)
	}
	if _, err := a.persistenceHandle().CommitEvents("repair-uncertain-start", session.ToolStarted{TurnID: "turn-0", Call: session.ToolCall{CallID: call.ID, ToolID: call.Name, Arguments: []byte(`{"file_path":"x"}`)}, Fingerprint: "repair-fingerprint"}); err != nil {
		t.Fatal(err)
	}
	if err := a.clearHistoryCommand("repair-discard-tools"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSession(a.services.cfg.SessionDir, a.SessionID())
	if err != nil || len(snapshot.History) != 0 {
		t.Fatalf("discarded tool round reappeared after reset: history=%+v, %v", snapshot.History, err)
	}
}
