package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestConcurrencyLimitsIndependentAndStrict(t *testing.T) {
	cfg := defaultConfig()
	if cfg.MaxParallelTools != 4 || cfg.MaxConcurrentAgents != 4 {
		t.Fatalf("defaults: tools=%d agents=%d", cfg.MaxParallelTools, cfg.MaxConcurrentAgents)
	}
	for _, field := range []string{"max_parallel_tools", "max_concurrent_agents"} {
		for _, limit := range []int{-1, 0, 1, 16, 17} {
			candidate := defaultConfig()
			source := Config{}
			if field == "max_parallel_tools" {
				source.MaxParallelTools = limit
			} else {
				source.MaxConcurrentAgents = limit
			}
			raw, _ := json.Marshal(limit)
			applyConfigFields(&source, &source, map[string]json.RawMessage{field: raw}, SourceProject, "settings.json", true)
			source.provenance.setProject("project", true, "test")
			ApplyProjectSettings(&candidate, &source)
			err := candidate.Validate()
			if (err == nil) != (limit >= 1 && limit <= 16) {
				t.Fatalf("%s=%d: %v", field, limit, err)
			}
			if field == "max_parallel_tools" && candidate.MaxConcurrentAgents != 4 || field == "max_concurrent_agents" && candidate.MaxParallelTools != 4 {
				t.Fatal("limits are coupled")
			}
		}
	}
}

func TestProjectMemoryDefaultsAndProtectedStorage(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.EnableProjectMemory || cfg.MemoryReadOnly || cfg.MemoryHistory || filepath.Base(cfg.ProjectMemoryDir) != "projects" {
		t.Fatal("unexpected memory defaults")
	}
	cfg.Workspace = t.TempDir()
	cfg.ProjectMemoryDir = filepath.Join(cfg.Workspace, "notes")
	if _, err := cfg.Sandbox().ResolveWrite(filepath.Join(cfg.ProjectMemoryDir, "memory.md")); err == nil {
		t.Fatal("ordinary tool can write project notes")
	}
}
