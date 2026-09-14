package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/tui"
)

func TestShutdownTUIModelHandlesPointerReturnedByUpdate(t *testing.T) {
	model := tea.Model(tui.NewWithClient(nil, t.TempDir(), false))
	model, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/quit")})
	model, quit := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if quit == nil {
		t.Fatal("/quit should return a quit command")
	}
	if _, ok := model.(*tui.Model); !ok {
		t.Fatalf("quit Update returned %T, want *tui.Model", model)
	}
	if agent, err := shutdownTUIModel(model); err != nil || agent != nil {
		t.Fatalf("pointer TUI shutdown returned agent=%v err=%v", agent, err)
	}
}

func TestReadBoundedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readBoundedFile(path, 10); err != nil || string(got) != "0123456789" {
		t.Fatalf("readBoundedFile exact limit = %q, %v", got, err)
	}
	if _, err := readBoundedFile(path, 9); err == nil {
		t.Fatal("readBoundedFile accepted content above limit")
	}
	fifo := filepath.Join(t.TempDir(), "prompt.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	started := time.Now()
	if _, err := readBoundedFile(fifo, 10); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("readBoundedFile FIFO error = %v, want regular-file rejection", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO check blocked for %s", elapsed)
	}
}

func TestApplyCLIOverridesUsesFlagPresenceForZeroFalseAndEmpty(t *testing.T) {
	fs := flag.NewFlagSet("ccdp-test", flag.ContinueOnError)
	maxTurns := fs.Int("max-turns", 0, "")
	maxBudget := fs.Float64("max-budget", 0, "")
	debug := fs.Bool("debug", false, "")
	noPersist := fs.Bool("no-session-persistence", false, "")
	allowed := fs.String("allowedTools", "", "")
	prompt := fs.String("system-prompt", "", "")
	if err := fs.Parse([]string{"--max-turns=0", "--max-budget=0", "--debug=false", "--no-session-persistence=false", "--allowedTools=", "--system-prompt="}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.MaxTurns = 7
	cfg.MaxBudgetUSD = 2.5
	cfg.Debug = true
	cfg.NoSessionPersistence = true
	cfg.AlwaysAllow = []string{"Bash:git status"}
	cfg.SystemPrompt = "from file"
	visited, err := applyCLIOverrides(&cfg, fs, map[string]any{
		"max-turns":              *maxTurns,
		"max-budget":             *maxBudget,
		"debug":                  *debug,
		"no-session-persistence": *noPersist,
		"allowedTools":           *allowed,
		"system-prompt":          *prompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"max-turns", "max-budget", "debug", "no-session-persistence", "allowedTools", "system-prompt"} {
		if !visited[name] {
			t.Fatalf("flag %q was not recorded as visited", name)
		}
	}
	if cfg.MaxTurns != 0 || cfg.MaxBudgetUSD != 0 || cfg.Debug || cfg.NoSessionPersistence || len(cfg.AlwaysAllow) != 0 || cfg.SystemPrompt != "" {
		t.Fatalf("explicit zero/false/empty values were not applied: %+v", cfg)
	}
	for _, field := range []string{"max_turns", "max_budget_usd", "debug", "no_session_persistence", "always_allow", "system_prompt"} {
		if cfg.SourceOf(field) != config.SourceCLI || !cfg.IsExplicit(field) {
			t.Fatalf("field %q provenance = %+v", field, cfg.SourceReport().Fields[field])
		}
	}

	// A fresh FlagSet with no visited flags must not overwrite file/env values
	// merely because its defaults happen to be zero or false.
	untouched := config.Default()
	untouched.MaxTurns = 7
	untouched.Debug = true
	if _, err := applyCLIOverrides(&untouched, flag.NewFlagSet("empty", flag.ContinueOnError), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if untouched.MaxTurns != 7 || !untouched.Debug || untouched.SourceOf("max_turns") == config.SourceCLI {
		t.Fatalf("unvisited flags changed config: %+v", untouched)
	}
}

func newMainLifecycleAgent(t *testing.T, root string) *agent.Agent {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace = root
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.APIKey = "test-key"
	cfg.BaseURL = "http://127.0.0.1:1"
	ag, err := agent.New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

func waitMainAgentClosed(t *testing.T, ag *agent.Agent) {
	t.Helper()
	select {
	case <-ag.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("agent was not closed during TUI shutdown")
	}
}

func TestResumeThenQuitShutsDownCurrentAgent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := newMainLifecycleAgent(t, root)
	defer source.Close()
	target := newMainLifecycleAgent(t, root)
	targetID := target.SessionID()
	if err := target.Save(); err != nil {
		t.Fatal(err)
	}
	if err := target.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	var model tea.Model = tui.NewWithResume(source, false)
	model, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/resume " + targetID)})
	model, open := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if open == nil {
		t.Fatal("/resume did not schedule independent session open")
	}
	opened, _ := model.Update(open())
	var current *agent.Agent
	switch value := opened.(type) {
	case tui.Model:
		current = value.CurrentAgent()
	case *tui.Model:
		if value != nil {
			current = value.CurrentAgent()
		}
	}
	if current == nil || current.SessionID() != targetID {
		t.Fatalf("resume selected current agent=%v, want %q", current, targetID)
	}
	returned, err := shutdownTUIModel(opened)
	if err != nil || returned != current {
		t.Fatalf("shutdown returned agent=%v err=%v, want resumed agent %p", returned, err, current)
	}
	if err := returned.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitMainAgentClosed(t, returned)
	waitMainAgentClosed(t, source)
}
