package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/session"
	"ccdp/internal/session/journal"
)

type pagingStore struct {
	session.Store
	reads atomic.Int64
}

func (s *pagingStore) Read(cursor session.Cursor) ([]session.Record, error) {
	s.reads.Add(1)
	return s.Store.Read(cursor)
}

type pagingArtifacts struct {
	journal.ArtifactAccess
	fullReads atomic.Int64
	pageBytes atomic.Int64
}

func (s *pagingArtifacts) Read(ref session.BlobRef, limit int64) ([]byte, error) {
	s.fullReads.Add(1)
	return s.ArtifactAccess.Read(ref, limit)
}

func (s *pagingArtifacts) ReadRange(ref session.BlobRef, offset int64, limit int) ([]byte, error) {
	data, err := s.ArtifactAccess.ReadRange(ref, offset, limit)
	s.pageBytes.Add(int64(len(data)))
	return data, err
}

func TestReadOutputPagesWithoutCopyingHistoryOrFullBlob(t *testing.T) {
	for _, memory := range []bool{false, true} {
		t.Run(fmt.Sprint(memory), func(t *testing.T) {
			a, _ := sessionRepairAgent(t, memory)
			ref := saveTestOutput(t, a, strings.Repeat("x", 8<<20))
			history := sessionRepairHistory(100)
			history[0].SourceOutputs = []messages.OutputReference{ref}
			sessionRepairSeed(t, a, history)
			backend := journalBackend(t, a.persistenceHandle())
			backend.mu.Lock()
			store := &pagingStore{Store: backend.store}
			blobs := &pagingArtifacts{ArtifactAccess: backend.blobs}
			backend.store, backend.blobs = store, blobs
			backend.mu.Unlock()
			for i := range 3 {
				page, err := a.readOutput(context.Background(), ref.Hash, int64(i*16384), 16384)
				if err != nil || len(page.Text) != 16384 || page.Next != int64((i+1)*16384) {
					t.Fatalf("page %d: %+v, %v", i, page, err)
				}
			}
			if store.reads.Load() != 0 || blobs.fullReads.Load() != 0 || blobs.pageBytes.Load() > 3*(16384+4) {
				t.Fatalf("paging did whole-state work: log reads=%d full reads=%d page bytes=%d", store.reads.Load(), blobs.fullReads.Load(), blobs.pageBytes.Load())
			}
		})
	}
}

func TestCompletedCommandOutputSurvivesLogRelease(t *testing.T) {
	a, _ := sessionRepairAgent(t, false)
	a.services.perms.SetMode(permissions.ModeAuto)
	command := "i=0; while [ \"$i\" -lt 1000 ]; do printf 'retained line\\n'; i=$((i+1)); done"
	call := messages.ToolCall{ID: "saved-log", Name: "Bash", Arguments: map[string]any{"command": command}}
	result := a.executeToolResultWithJournal(call, toolJournalContextForStep(testToolStep(t, a)))
	if result.IsError || result.Process == nil || !result.Process.Terminal() || result.OutputID != result.Process.OutputID {
		t.Fatalf("missing durable command log: %+v", result)
	}
	stop := messages.ToolCall{ID: "release-log", Name: "Process", Arguments: map[string]any{"id": result.Process.ID, "action": "stop"}}
	stopped := a.executeToolResultWithJournal(stop, toolJournalContextForStep(testToolStep(t, a)))
	if stopped.IsError {
		t.Fatal(stopped.Output)
	}
	if _, err := os.Stat(result.Process.Log); !os.IsNotExist(err) {
		t.Fatalf("temporary log was not released: %v", err)
	}
	page, err := a.readOutput(context.Background(), result.OutputID, 0, 65536)
	if err != nil || page.More || page.Text != strings.Repeat("retained line\n", 1000) {
		t.Fatalf("completed output was lost after release: bytes=%d, %v", len(page.Text), err)
	}
}

func saveTestOutput(t *testing.T, a *Agent, text string) messages.OutputReference {
	t.Helper()
	blobs, err := a.persistenceHandle().RequestArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := blobs.PutContext(context.Background(), []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return messages.OutputReference{Hash: ref.Hash, Size: ref.Size, MediaType: "text/plain"}
}

func TestReadOutputSurvivesForkAndSourceClose(t *testing.T) {
	for _, memory := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory=%t", memory), func(t *testing.T) {
			a, cfg := sessionRepairAgent(t, memory)
			text := "中文原文\n" + strings.Repeat("full output\n", 1000)
			ref := saveTestOutput(t, a, text)
			// A compacted summary must carry references even though the original
			// tool messages are no longer in the forked history.
			if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "earlier result: " + ref.Hash,
				SourceOutputs: []messages.OutputReference{ref}}); err != nil {
				t.Fatal(err)
			}
			unreferenced := saveTestOutput(t, a, "request secret, not tool output")
			if _, err := a.readOutput(context.Background(), unreferenced.Hash, 0, 10); err == nil {
				t.Fatal("unreferenced artifact exposed")
			}
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
			var restored strings.Builder
			var offset int64
			for {
				page, err := fork.readOutput(context.Background(), ref.Hash, offset, 7)
				if err != nil {
					t.Fatal(err)
				}
				restored.WriteString(page.Text)
				if page.Total != int64(len(text)) || page.Next <= offset && page.More {
					t.Fatalf("bad cursor: %+v", page)
				}
				offset = page.Next
				if !page.More {
					break
				}
			}
			if restored.String() != text {
				t.Fatal("fork lost original output")
			}
			if _, err := fork.readOutput(context.Background(), ref.Hash, 1, 10); err == nil {
				t.Fatal("accepted a cursor inside a UTF-8 character")
			}
			if memory {
				if _, err := os.Stat(cfg.SessionDir); !os.IsNotExist(err) {
					t.Fatalf("NoSession wrote a directory: %v", err)
				}
			}
		})
	}
}

func TestPruneToolOutputsPreservesUnsafeAndRecentGroups(t *testing.T) {
	a, _ := sessionRepairAgent(t, true)
	text := strings.Repeat("original data ", 1000)
	ref := saveTestOutput(t, a, text)
	var history []messages.Message
	for i := 0; i < 7; i++ {
		call := messages.ToolCall{ID: fmt.Sprint(i), Name: "Read", Arguments: map[string]any{}}
		assistant := messages.AssistantWithTools("next", []messages.ToolCall{call})
		assistant.ProviderContent = []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"reason","signature":"signed"}`)}
		result := messages.NewToolResult(call, text, i == 1)
		copy := ref
		result.OutputRef = &copy
		if i == 2 {
			result.OutputRef.Hash = strings.Repeat("0", 64) // valid identity, missing data
		}
		if i == 3 {
			result.ProviderContent = []json.RawMessage{json.RawMessage(`{"opaque":"untouched"}`)}
		}
		history = append(history, assistant, result)
	}
	history = append(history, messages.Message{Role: messages.RoleUser, Content: "latest exact request"},
		messages.AssistantWithTools("pending", []messages.ToolCall{{ID: "pending", Name: "Read"}}))
	before := messages.Clone(history)
	cleaned, changed := a.pruneToolOutputs(context.Background(), history, 4)
	if !changed || !strings.Contains(cleaned[1].Content, ref.Hash) || !cleaned[1].DisplayTruncated {
		t.Fatal("old successful output was not replaced with a usable reference")
	}
	for _, index := range []int{0, 3, 5, 7, len(history) - 3, len(history) - 2, len(history) - 1} {
		if !reflect.DeepEqual(cleaned[index], history[index]) {
			t.Fatalf("changed protected message %d", index)
		}
	}
	if !reflect.DeepEqual(before, history) {
		t.Fatal("candidate mutated original history")
	}
}

func TestFailedSummaryDoesNotCommitPrunedCandidate(t *testing.T) {
	provider := &childRuntimeTestProvider{name: "incomplete-summary", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "incomplete", FinishReason: "length"}, nil
	}}
	a := newJournalRuntimeAgent(t, provider)
	output := strings.Repeat("old successful output ", 1000)
	ref := saveTestOutput(t, a, output)
	for i := 0; i < 6; i++ {
		call := messages.ToolCall{ID: fmt.Sprint(i), Name: "Read", Arguments: map[string]any{}}
		result := messages.NewToolResult(call, output, false)
		result.OutputRef = &ref
		if err := a.appendHistory(messages.AssistantWithTools("", []messages.ToolCall{call}), result); err != nil {
			t.Fatal(err)
		}
	}
	before := a.History()
	if err := a.compactContext(context.Background()); err == nil {
		t.Fatal("incomplete summary accepted")
	}
	if !reflect.DeepEqual(before, a.History()) {
		t.Fatal("failed summary committed a partially prepared replacement")
	}
}

func TestForkBranchSummaryKeepsAbandonedOutputReferences(t *testing.T) {
	provider := &childRuntimeTestProvider{name: "branch-output-summary", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		return llm.StreamResult{Text: "A result from the abandoned direction remains useful.", FinishReason: "stop"}, nil
	}}
	a := newJournalRuntimeAgent(t, provider)
	ref := saveTestOutput(t, a, "original abandoned result")
	call := messages.ToolCall{ID: "abandoned", Name: "Read", Arguments: map[string]any{}}
	result := messages.NewToolResult(call, "preview", false)
	result.OutputRef = &ref
	if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "fork here"},
		messages.AssistantWithTools("", []messages.ToolCall{call}), result); err != nil {
		t.Fatal(err)
	}
	id, err := a.Fork(1)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := a.OpenSession(id)
	if err != nil {
		t.Fatal(err)
	}
	defer fork.Close()
	if err := a.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := fork.readOutput(context.Background(), ref.Hash, 0, 1024)
	if err != nil || page.Text != "original abandoned result" {
		t.Fatalf("branch summary dropped source output: %+v %v", page, err)
	}
}

func TestContextPreparationCountsWholeRequestAndCanSkipSummary(t *testing.T) {
	var summaries int
	provider := &childRuntimeTestProvider{name: "no-summary-needed", stream: func(context.Context, llm.CompletionRequest, func(string)) (llm.StreamResult, error) {
		summaries++
		return llm.StreamResult{}, errors.New("summary should not be needed")
	}}
	a := newJournalRuntimeAgent(t, provider)
	a.services.cfg.ContextWindow = 8000
	a.services.cfg.CompactThreshold = .8
	a.services.cfg.KeepAfterCompact = 4
	oldOutput := strings.Repeat("data evidence ", 2000)
	ref := saveTestOutput(t, a, oldOutput)
	for i := 0; i < 4; i++ {
		call := messages.ToolCall{ID: fmt.Sprint(i), Name: "Read", Arguments: map[string]any{}}
		result := messages.NewToolResult(call, oldOutput, false)
		result.OutputRef = &ref
		if err := a.appendHistory(messages.AssistantWithTools("inspect", []messages.ToolCall{call}), result); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if err := a.appendHistory(messages.Message{Role: messages.RoleUser, Content: "keep this recent constraint"}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := a.configSnapshot()
	req := a.buildRequestFromConfig(cfg, a.currentSandbox().Snapshot(), cfg.Model, "instructions", a.History(), nil)
	req.MaxTokens = intPtr(512)
	if !a.needsContextPreparation(req) {
		t.Fatal("large frozen request not recognized")
	}
	changed, err := a.prepareContextForRequest(context.Background(), req, false)
	if err != nil || !changed || summaries != 0 {
		t.Fatalf("cleanup failed or unnecessarily summarized: changed=%t summaries=%d err=%v", changed, summaries, err)
	}
	before := a.History()
	tooLarge := llm.CompletionRequest{Messages: []llm.ChatMessage{{Role: "system", Content: strings.Repeat("fixed instructions ", 10000)}}}
	if !a.needsContextPreparation(tooLarge) {
		t.Fatal("system instructions omitted from threshold")
	}
	if changed, err := a.prepareContextForRequest(context.Background(), tooLarge, true); changed || !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("irreducible request should fail directly: changed=%t err=%v", changed, err)
	}
	if summaries != 0 || !reflect.DeepEqual(before, a.History()) {
		t.Fatal("irreducible fixed context changed history or requested a summary")
	}
}
