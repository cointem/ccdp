package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"ccdp/internal/workspace"
)

func TestLSPPolicyHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "lsp-policy-helper" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		size := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				os.Exit(0)
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "Content-Length:") {
				size, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:")))
			}
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(reader, body); err != nil {
			os.Exit(2)
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			os.Exit(3)
		}
		if request.ID == 0 {
			continue
		}
		var result any = map[string]any{"capabilities": map[string]any{}}
		if request.Method == "textDocument/documentSymbol" {
			result = []any{}
		}
		reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(reply), reply)
	}
}

func TestCodeNavigateRecordsExternalExecutionBeforePolicyTightening(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires Seatbelt")
	}
	cfg := isolatedTestConfig(t, "lsp-policy")
	writable := t.TempDir()
	document := filepath.Join(cfg.Workspace, "main.go")
	if err := os.WriteFile(document, []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AdditionalDirectories = []string{writable}
	cfg.PermissionMode = "auto"
	cfg.EnableGuardian = config.BoolPtr(false)
	cfg.LanguageServers = map[string]workspace.LanguageServer{
		"test": {Argv: []string{executable, "-test.run=^TestLSPPolicyHelper$", "--", "lsp-policy-helper"}, LanguageID: "go", Extensions: []string{".go"}},
	}
	a, err := testNewWithOptions(t, &cfg, Options{EffectiveConfigFrozen: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	frozen := a.services.sandbox.Snapshot()
	if _, err := a.semanticNavigate(context.Background(), cfg, frozen, "outline", document, 1, 0); err != nil {
		t.Fatal(err)
	}
	if !frozen.ExternalExecutionPossible() || !a.services.sandbox.ExternalExecutionPossible() {
		t.Fatal("LSP execution was not recorded on the shared policy")
	}
	cmd := protocol.Command{
		ID: "tighten-after-lsp", Type: protocol.CommandSetSandboxPolicy,
		SandboxPolicy: &protocol.SetSandboxPolicy{Policy: protocol.SandboxPolicy{
			NetworkAccess: cfg.NetworkAccess, AdditionalDirectories: []string{writable}, DisallowedDirectories: []string{writable},
		}},
	}
	receipt := submitTestCommand(t, a, cmd)
	if receipt.Status == protocol.ReceiptScheduled {
		receipt = waitFormalCommandReceipt(t, a, cmd.ID)
	}
	if !receipt.Rejected() {
		t.Fatalf("tightening accepted after arbitrary LSP execution: %+v", receipt)
	}
	if _, err := a.services.sandbox.ResolveWrite(filepath.Join(writable, "file")); err != nil {
		t.Fatalf("rejected tightening changed the published policy: %v", err)
	}
}
