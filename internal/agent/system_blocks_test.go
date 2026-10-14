package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ccdp/internal/llm"
	"ccdp/internal/messages"
	"ccdp/internal/workspace"
)

// TestSystemBlockClassification verifies buildSystemBlocks emits ordered,
// cache-offset sections: stable sections (core, tools, project instructions)
// lead and are cacheable, while the environment section is dynamic.
func TestSystemBlockClassification(t *testing.T) {
	ag := newAgentForTest(t)
	defer ag.Close()

	cfg := cloneConfig(ag.services.cfg)
	cfg.Workspace = t.TempDir() // CachedScan yields an (empty) repo map block.

	blocks := ag.buildSystemBlocks(cfg, time.Now(), "session-x", false, "", false,
		"PROJECT_RULES_MARKER", "SKILLS_MARKER",
		[]llm.ToolDef{{Function: llm.FuncDef{Name: "read_file"}}}, ag.History(), ag.currentSandbox().Snapshot())

	if len(blocks) == 0 {
		t.Fatal("no system blocks produced")
	}
	if blocks[0].Text != cfg.SystemPrompt || !blocks[0].Cacheable {
		t.Fatalf("first block must be the cacheable core prompt: %+v", blocks[0])
	}

	var projectFound, envFound bool
	for _, b := range blocks {
		switch {
		case strings.Contains(b.Text, "PROJECT_RULES_MARKER"):
			projectFound = true
			if !b.Cacheable {
				t.Fatalf("project instructions must be a cacheable section: %+v", b)
			}
		case strings.Contains(b.Text, "SKILLS_MARKER"):
			if !b.Cacheable {
				t.Fatalf("skills index must be a cacheable section: %+v", b)
			}
		case strings.Contains(b.Text, "# Environment"):
			envFound = true
			if b.Cacheable {
				t.Fatalf("environment section must be dynamic (uncached): %+v", b)
			}
		}
	}
	if !projectFound {
		t.Fatalf("project instructions missing from blocks")
	}
	if !envFound {
		t.Fatalf("environment section missing from blocks")
	}
}

func TestSystemBlockStablePrefixSurvivesRepositoryQueryChange(t *testing.T) {
	ag := newAgentForTest(t)
	defer ag.Close()
	cfg := cloneConfig(ag.services.cfg)
	// Exceed the repository map's 120-entry budget so relevance changes which
	// files are selected, rather than merely sorting the same complete list.
	for i := 0; i < 122; i++ {
		if err := os.WriteFile(filepath.Join(cfg.Workspace, fmt.Sprintf("file%03d.go", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	workspace.Invalidate(cfg.Workspace)
	started := time.Now()
	build := func(query string) ([]llm.SystemBlock, string) {
		ag.mu.Lock()
		ag.conversation.durable.History = []messages.Message{{Role: messages.RoleUser, Content: query}}
		ag.mu.Unlock()
		blocks := ag.buildSystemBlocks(cfg, started, "session", false, "", false,
			"PROJECT_RULES_MARKER", "SKILLS_MARKER", []llm.ToolDef{{Function: llm.FuncDef{Name: "read_file"}}}, ag.History(), ag.currentSandbox().Snapshot())
		var stable []llm.SystemBlock
		var layout string
		dynamic := false
		for _, b := range blocks {
			if b.Cacheable {
				if dynamic {
					t.Fatal("cacheable instructions follow dynamic content")
				}
				stable = append(stable, b)
			} else {
				dynamic = true
			}
			if strings.Contains(b.Text, "# Repository layout") {
				layout = b.Text
			}
		}
		return stable, layout
	}
	first, firstLayout := build("file120")
	second, secondLayout := build("file121")
	if len(first) != 4 || !reflect.DeepEqual(first, second) {
		t.Fatalf("stable prefix changed: first=%+v second=%+v", first, second)
	}
	if first[0].Text != cfg.SystemPrompt || !strings.Contains(first[1].Text, "read_file") || first[2].Text != "SKILLS_MARKER" || !strings.Contains(first[3].Text, "PROJECT_RULES_MARKER") {
		t.Fatal("stable instructions changed priority")
	}
	if firstLayout == "" || secondLayout == "" || firstLayout == secondLayout {
		t.Fatal("fixture did not change the query-dependent repository map")
	}
}
