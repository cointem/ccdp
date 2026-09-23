package agent

import (
	"strings"
	"testing"
	"time"

	"ccdp/internal/llm"
)

// TestSystemBlockClassification verifies buildSystemBlocks emits ordered,
// cache-offset sections: stable sections (core, tools, project instructions)
// lead and are cacheable, while the environment section is dynamic.
func TestSystemBlockClassification(t *testing.T) {
	ag := newAgentForTest(t)
	defer ag.Close()

	cfg := cloneConfig(ag.cfg)
	cfg.Workspace = t.TempDir() // CachedScan yields an (empty) repo map block.

	blocks := ag.buildSystemBlocks(cfg, time.Now(), "session-x", false, "", false,
		"PROJECT_RULES_MARKER", "SKILLS_MARKER",
		[]llm.ToolDef{{Function: llm.FuncDef{Name: "read_file"}}})

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