package config

import (
	"encoding/json"
	"testing"
)

func TestProjectConcurrencyRequiresTrustToIncrease(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, limit := range []int{2, 16} {
			project := Config{MaxParallelTools: limit, MaxConcurrentAgents: limit}
			raw, _ := json.Marshal(limit)
			applyConfigFields(&project, &project, map[string]json.RawMessage{"max_parallel_tools": raw, "max_concurrent_agents": raw}, SourceProject, "settings.json", trusted)
			project.provenance.setProject("project", trusted, "test")
			for _, source := range []Source{SourceDefault, SourceEnvironment, SourceCLI} {
				cfg := Default()
				cfg.markSource("max_parallel_tools", source, "", true)
				cfg.markSource("max_concurrent_agents", source, "", true)
				ApplyProjectSettings(&cfg, &project)
				want := 4
				if source == SourceDefault && (trusted || limit < 4) {
					want = limit
				}
				if cfg.MaxParallelTools != want || cfg.MaxConcurrentAgents != want {
					t.Fatalf("trusted=%v limit=%d source=%s: got %d/%d want %d", trusted, limit, source, cfg.MaxParallelTools, cfg.MaxConcurrentAgents, want)
				}
			}
		}
	}
}
