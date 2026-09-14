package main

import (
	"context"
	"flag"
	"testing"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
)

func TestGenerationCLIOverrides(t *testing.T) {
	fs := flag.NewFlagSet("generation", flag.ContinueOnError)
	effort := fs.String("effort", "", "")
	verbosity := fs.String("verbosity", "", "")
	if err := fs.Parse([]string{"--effort=high", "--verbosity="}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Verbosity = "low"
	if _, err := applyCLIOverrides(&cfg, fs, map[string]any{"effort": *effort, "verbosity": *verbosity}); err != nil {
		t.Fatal(err)
	}
	if cfg.ReasoningEffort != "high" || cfg.Verbosity != "" {
		t.Fatal("generation CLI presence not honored")
	}
}

func TestHeadlessQuestionSnapshotAndEventCancelOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := &headlessTestClient{snapshot: headlessTestSnapshot()}
	watch := &headlessWatch{client: client}
	if err := watch.open(ctx); err != nil {
		t.Fatal(err)
	}
	defer watch.close()
	question := &protocol.QuestionRequest{ID: "q"}
	snapshot := client.snapshot
	snapshot.Question = question
	client.publish(protocol.Update{Type: protocol.UpdateState, Snapshot: &snapshot})
	client.publish(protocol.Update{Type: protocol.UpdateStream, Event: &protocol.EventView{Kind: protocol.EventQuestionRequest, Question: question}})
	client.publish(protocol.Update{Type: protocol.UpdateStream, Event: &protocol.EventView{Kind: protocol.EventTurnDone}})
	if err := consumeHeadlessTurn(ctx, ctx, watch, snapshot.SessionID, outText, &turnSink{}, 0, client); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.commands) != 1 || client.commands[0].Type != protocol.CommandAnswerQuestion || !client.commands[0].Answer.Cancelled {
		t.Fatalf("headless answer commands: %+v", client.commands)
	}
}
