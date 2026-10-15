package config

import (
	"encoding/json"
	"testing"
)

func TestNetworkProxyUserEnvironmentAndProjectBoundary(t *testing.T) {
	cfg := Default()
	src := Config{NetworkProxy: "http://127.0.0.1:7897"}
	raw := map[string]json.RawMessage{"network_proxy": json.RawMessage(`"http://127.0.0.1:7897"`)}
	applyConfigFields(&cfg, &src, raw, SourceUserFile, "config.json", true)
	if cfg.NetworkProxy != src.NetworkProxy || cfg.Sandbox().Snapshot().NetworkProxy != src.NetworkProxy {
		t.Fatal("user proxy lost in config/sandbox")
	}
	project := Config{NetworkProxy: "http://127.0.0.1:9999"}
	applyConfigFields(&project, &project, raw, SourceProject, "settings.json", true)
	ApplyProjectSettings(&cfg, &project)
	if cfg.NetworkProxy != src.NetworkProxy {
		t.Fatal("project replaced trusted user proxy")
	}
	t.Setenv("CCDP_NETWORK_PROXY", "http://127.0.0.1:8888")
	if err := applyEnvChecked(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkProxy != "http://127.0.0.1:8888" {
		t.Fatal("environment override lost")
	}
	cfg.NetworkProxy = "http://untrusted.example:80"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-loopback proxy accepted")
	}
}
