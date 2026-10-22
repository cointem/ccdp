package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

func TestBashCreatedFileRemainsReadableAfterSettingsChanges(t *testing.T) {
	for _, change := range []string{"reload", "workspace", "sandbox"} {
		t.Run(change, func(t *testing.T) {
			cfg := isolatedTestConfig(t, "bash-read-environment")
			a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)
			scratch := a.services.resources.ScratchDir()
			scratch, err = filepath.EvalSymlinks(scratch)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "workspace":
				if err := a.SetWorkspace(t.TempDir()); err != nil {
					t.Fatal(err)
				}
			default:
				cmd := protocol.Command{ID: "change-for-bash-read", Type: protocol.CommandReloadSettings, Reload: &protocol.ReloadCommand{}}
				if change == "sandbox" {
					cmd.Type, cmd.Reload = protocol.CommandSetSandboxPolicy, nil
					cmd.SandboxPolicy = &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{NetworkAccess: cfg.NetworkAccess, AdditionalDirectories: []string{t.TempDir()}}}
				}
				receipt := submitTestCommand(t, a, cmd)
				if receipt.Status == protocol.ReceiptScheduled {
					receipt = waitFormalCommandReceipt(t, a, cmd.ID)
				}
				if receipt.Rejected() {
					t.Fatalf("settings rejected: %+v", receipt)
				}
			}
			policy := a.services.sandbox.Snapshot()
			if len(policy.ScratchDirs) != 1 || policy.ScratchDirs[0] != scratch {
				t.Fatalf("session scratch lost after %s: %v", change, policy.ScratchDirs)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tctx := a.buildToolContext(messages.ToolCall{}, ctx, a.configSnapshot(), policy, a.services.resources, nil)
			result, err := tools.NewBashTool().Execute(tctx, map[string]any{"command": `printf 'written by bash\n' > "$TMPDIR/read-back.txt"; printf 'retained log\n'`, "yield_ms": 1000})
			if err != nil {
				t.Fatal(err)
			}
			var process struct {
				ID       int    `json:"process_id"`
				Status   string `json:"status"`
				ExitCode *int   `json:"exit_code"`
				Log      string `json:"log_path"`
				Next     int    `json:"next_offset"`
			}
			for {
				if err := json.Unmarshal([]byte(result.Text), &process); err != nil {
					t.Fatal(err)
				}
				if process.Status != "running" {
					break
				}
				result, err = tools.NewProcessTool().Execute(tctx, map[string]any{"id": process.ID, "action": "read", "offset": process.Next, "wait_ms": 1000})
				if err != nil {
					t.Fatal(err)
				}
			}
			if process.ExitCode == nil || *process.ExitCode != 0 {
				t.Fatalf("Bash failed: %s", result.Text)
			}
			if !filepath.IsAbs(process.Log) {
				t.Fatalf("log path is not absolute: %q", process.Log)
			}
			result, err = tools.NewReadTool().Execute(tctx, map[string]any{"file_path": process.Log})
			if err != nil || !strings.Contains(result.Text, "retained log") {
				t.Fatalf("Read could not read retained Bash log: %s %v", result.Text, err)
			}
			result, err = tools.NewReadTool().Execute(tctx, map[string]any{"file_path": filepath.Join(scratch, "read-back.txt")})
			if err != nil || !strings.Contains(result.Text, "written by bash") {
				t.Fatalf("Read could not read Bash file: %s %v", result.Text, err)
			}
		})
	}
}
