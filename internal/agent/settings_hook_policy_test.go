package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/hooks"
	"ccdp/internal/protocol"
)

func TestSettingsPublishHookSandbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("hook execution requires the macOS Seatbelt backend")
	}
	for _, entry := range []string{"immediate", "reload"} {
		t.Run(entry, func(t *testing.T) {
			cfg := isolatedTestConfig(t, "hook-policy-publication")
			blocked := filepath.Join(cfg.Workspace, "hook-output")
			if err := os.Mkdir(blocked, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(blocked, "marker")
			cfg.Hooks = hooks.Config{
				hooks.EventPreToolUse: {{Command: formatExternalCommand([]string{"touch", marker})}},
			}
			cfg.TrustStore = config.NewTrustStore(filepath.Join(t.TempDir(), "project-trust.json"))
			a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)

			setDenied := func(dirs []string) {
				t.Helper()
				cmd := protocol.Command{
					ID: protocol.CommandID(nextRuntimeID("hook-policy")), Type: protocol.CommandSetSandboxPolicy,
					SandboxPolicy: &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{
						NetworkAccess: cfg.NetworkAccess, DisallowedDirectories: dirs,
					}},
				}
				if entry == "reload" {
					data, err := json.Marshal(map[string]any{"disallowed_directories": dirs})
					if err != nil {
						t.Fatal(err)
					}
					settingsDir := filepath.Join(cfg.Workspace, ".ccdp")
					if err := os.MkdirAll(settingsDir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), data, 0600); err != nil {
						t.Fatal(err)
					}
					cmd.Type, cmd.SandboxPolicy, cmd.Reload = protocol.CommandReloadSettings, nil, &protocol.ReloadCommand{}
				}
				receipt := submitTestCommand(t, a, cmd)
				if receipt.Status == protocol.ReceiptScheduled {
					receipt = waitFormalCommandReceipt(t, a, cmd.ID)
				}
				if receipt.Rejected() {
					t.Fatalf("policy publication: %+v", receipt)
				}
			}

			if _, err := a.services.sandbox.ResolveWrite(marker); err != nil {
				t.Fatalf("initial policy: %v", err)
			}
			// Tighten before launching a hook: executing a subprocess records the
			// session witness that deliberately prevents later policy tightening.
			setDenied([]string{blocked})
			if _, err := a.services.sandbox.ResolveWrite(marker); err == nil {
				t.Fatal("published sandbox still permits the denied path")
			}
			result := a.services.hooks.PreToolUse(context.Background(), "Read", nil)
			if result.Decision != hooks.DecisionDeny {
				t.Fatalf("hook did not use the tightened policy: %+v", result)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("hook wrote the denied path: stat=%v, result=%+v", err, result)
			}

			setDenied([]string{})
			if _, err := a.services.sandbox.ResolveWrite(marker); err != nil {
				t.Fatalf("widened sandbox still denies the path: %v", err)
			}
			result = a.services.hooks.PreToolUse(context.Background(), "Read", nil)
			if result.Decision != hooks.DecisionNone {
				t.Fatalf("hook did not use the widened policy: %+v", result)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("hook failed to write after widening: %v, result=%+v", err, result)
			}
		})
	}
}
