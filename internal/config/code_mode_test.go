package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCodeModeConfigDefaultEnabled(t *testing.T) {
	if !Default().CodeModeEnabled {
		t.Fatal("Code Mode must be enabled by default")
	}
	for _, tc := range []struct {
		name     string
		contents string
		enabled  bool
		explicit bool
	}{
		{name: "missing file", enabled: true},
		{name: "omitted field", contents: `{}`, enabled: true},
		{name: "explicit true", contents: `{"code_mode_enabled":true}`, enabled: true, explicit: true},
		{name: "explicit false", contents: `{"code_mode_enabled":false}`, explicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tc.contents != "" {
				if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := LoadFrom(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CodeModeEnabled != tc.enabled || cfg.IsExplicit("code_mode_enabled") != tc.explicit {
				t.Fatalf("enabled=%v explicit=%v; want enabled=%v explicit=%v", cfg.CodeModeEnabled, cfg.IsExplicit("code_mode_enabled"), tc.enabled, tc.explicit)
			}
		})
	}
}

func TestCodeModeOmittedProjectSettingInheritsUserConfig(t *testing.T) {
	root := t.TempDir()
	project, err := LoadProjectSettings(root)
	if err != nil {
		t.Fatal(err)
	}
	if project.CodeModeEnabled || project.IsExplicit("code_mode_enabled") {
		t.Fatal("omitted project setting must not introduce a capability grant")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	for _, enabled := range []bool{true, false} {
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"code_mode_enabled":%t}`, enabled)), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadFrom(path)
		if err != nil {
			t.Fatal(err)
		}
		ApplyProjectSettings(&cfg, &project)
		if cfg.CodeModeEnabled != enabled {
			t.Fatalf("omitted project setting changed user value %v", enabled)
		}
	}
}
