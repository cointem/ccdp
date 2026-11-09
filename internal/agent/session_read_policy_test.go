package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/permissions"
	"ccdp/internal/sandbox"
	"ccdp/internal/session/archive"
	"ccdp/internal/tools"
)

func TestOtherSessionDataIsPrivate(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, rebuild := range []bool{false, true} {
			t.Run(fmt.Sprintf("nested=%v/rebuild=%v", nested, rebuild), func(t *testing.T) {
				workspace := t.TempDir()
				sessions := filepath.Join(t.TempDir(), "sessions")
				if nested {
					sessions = filepath.Join(workspace, ".ccdp", "sessions")
				}
				cfg := &config.Config{Workspace: workspace, SessionDir: sessions}
				makePolicy := func() *sandbox.Sandbox {
					if rebuild {
						return buildSandbox(cfg, workspace)
					}
					return cfg.Sandbox()
				}
				path := filepath.Join(sessions, "other-session", "outputs", "web.txt")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("saved output\n"), 0600); err != nil {
					t.Fatal(err)
				}
				sb := makePolicy()
				r := tools.NewResources("current", filepath.Join(sessions, "current"), t.TempDir()+"/runtime")
				defer r.Close()
				ctx := r.Context(nil, workspace, sb)
				ctx.Args = map[string]any{"file_path": path}
				if out, err := executeToolText(tools.NewReadTool(), ctx); err == nil {
					t.Fatalf("other session data was exposed: %q", out)
				}
				ctx.Args = map[string]any{"file_path": path, "mode": "replace", "content": "changed"}
				if _, err := executeToolText(tools.NewWriteTool(), ctx); err == nil {
					t.Fatal("session state became writable")
				}
				cfg.DisallowedDirectories = []string{sessions}
				ctx = r.Context(nil, workspace, makePolicy())
				ctx.Args = map[string]any{"file_path": path}
				if _, err := executeToolText(tools.NewReadTool(), ctx); err == nil {
					t.Fatal("explicit user denial was ignored")
				}
			})
		}
	}
}

func TestWebFetchContinuationReadableAfterSessionResume(t *testing.T) {
	body := strings.Repeat("preview 中文 ", 1000) + "\nlast line\n"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, body)
	}))
	defer proxy.Close()
	a, cfg := sessionRepairAgent(t, false)
	a.services.cfg.NetworkProxy = proxy.URL
	a.services.sandbox.NetworkProxy = proxy.URL
	a.services.perms.SetMode(permissions.ModeAuto)
	a.services.registry.Register(tools.NewWebFetchTool())
	call := messages.ToolCall{ID: "web-full-output", Name: "WebFetch", Arguments: map[string]any{"url": "http://public.invalid/article", "max_chars": 256}}
	if err := a.appendHistory(messages.AssistantWithTools("fetch", []messages.ToolCall{call})); err != nil {
		t.Fatal(err)
	}
	result := a.executeToolResultWithJournal(call, toolJournalContextForStep(testToolStep(t, a)))
	if result.IsError || !result.DisplayTruncated || result.OutputID == "" || !strings.Contains(result.Output, "ReadOutput output_id=") {
		t.Fatalf("missing shared continuation: %+v", result)
	}
	if strings.Contains(result.Output, "last line") {
		t.Fatal("max_chars did not limit the preview")
	}
	a.Close()
	snapshot, err := archive.Load(cfg.SessionDir, a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Resume(&cfg, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(opened.Close)
	page, err := opened.readOutput(context.Background(), result.OutputID, 0, 65536)
	if err != nil || !strings.HasSuffix(page.Text, body) || page.More {
		t.Fatalf("resume lost full output: bytes=%d, %v", len(page.Text), err)
	}
	forkID, err := opened.Fork(-1)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := opened.OpenSession(forkID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fork.Close)
	opened.Close()
	copied, err := fork.readOutput(context.Background(), result.OutputID, 0, 65536)
	if err != nil || copied.Text != page.Text {
		t.Fatalf("fork lost full output: %v", err)
	}
}
