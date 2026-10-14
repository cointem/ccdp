package agent

import (
	"os"
	"path/filepath"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/permissions"
)

func TestReviewRound3ResumeHonorsCLISettings(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.EnableGuardian = config.BoolPtr(false)
	cfg.PermissionMode = string(permissions.ModeBypass)
	cfg.AlwaysDeny = []string{"Bash:rm"}

	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.persistSettingsFact(a.sessionSettingsLocked(), a.sequences.settingsRev); err != nil {
		t.Fatal(err)
	}
	id := a.SessionID()
	if err := a.CloseContext(nil); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadSession(cfg.SessionDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Settings == nil {
		t.Fatal("fixture did not persist settings")
	}
	for field, value := range map[string]any{"permission_mode": "default", "always_deny": []string{"Write"}, "reasoning_effort": "high"} {
		if err := cfg.ApplyCLIOverride(field, value); err != nil {
			t.Fatal(err)
		}
	}
	resumed, err := Resume(&cfg, saved)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if mode := resumed.services.perms.CurrentMode(); mode != permissions.ModeDefault {
		t.Errorf("explicit --mode default became %q", mode)
	}
	decision, _ := resumed.services.perms.Check("Write", map[string]any{"file_path": "blocked.txt"}, permissions.KindWrite)
	if decision != permissions.DecisionDeny {
		t.Errorf("explicit --disallowedTools Write was lost; decision=%v", decision)
	}
	if got := resumed.services.cfg.ReasoningEffort; got != "high" {
		t.Errorf("explicit --effort high became %q", got)
	}
}

func TestReviewRound3OpenSessionDropsPreviousProjectTools(t *testing.T) {
	cfg := testPersistenceConfig(t)
	cfg.EnableMemory = config.BoolPtr(false)
	cfg.EnableGuardian = config.BoolPtr(false)
	trust := config.NewTrustStore(filepath.Join(t.TempDir(), "trust.json"))
	cfg.TrustStore = trust
	cfgB := cfg.Clone()
	cfgB.Workspace = t.TempDir()
	b, err := testNew(t, &cfgB)
	if err != nil {
		t.Fatal(err)
	}
	bid := b.SessionID()
	if err = b.CloseContext(nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.Workspace, ".ccdp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"tools":[{"name":"ProjectATool","command":"printf project-a","description":"Only for project A"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	project, err := config.LoadProjectSettings(cfg.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.AuthorizeProject(cfg.Workspace, project); err != nil {
		t.Fatal(err)
	}
	a, err := testNew(t, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, ok := a.services.registry.Get("ProjectATool"); !ok {
		t.Fatal("fixture did not load project tool")
	}
	opened, err := a.OpenSession(bid)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if opened.services.cfg.Workspace != cfgB.Workspace {
		t.Fatalf("wrong workspace: %s", opened.services.cfg.Workspace)
	}
	if _, ok := opened.services.registry.Get("ProjectATool"); ok {
		t.Error("project A's executable tool leaked into project B's resumed session")
	}
}
