package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ccdp/internal/config"
	"ccdp/internal/sandbox"
	"ccdp/internal/tools"
)

func TestSessionDataReadableButProtectedFromWrites(t *testing.T) {
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
				r := tools.NewResources("current", filepath.Join(sessions, "current"))
				defer r.Close()
				ctx := r.Context(nil, workspace, sb)
				ctx.Args = map[string]any{"file_path": path}
				if out, err := tools.NewReadTool().Run(ctx); err != nil || !strings.Contains(out, "saved output") {
					t.Fatalf("session read: %q, %v", out, err)
				}
				ctx.Args = map[string]any{"file_path": path, "mode": "replace", "content": "changed"}
				if _, err := tools.NewWriteTool().Run(ctx); err == nil {
					t.Fatal("session state became writable")
				}
				cfg.DisallowedDirectories = []string{sessions}
				ctx.Sandbox = makePolicy()
				ctx.Args = map[string]any{"file_path": path}
				if _, err := tools.NewReadTool().Run(ctx); err == nil {
					t.Fatal("explicit user denial was ignored")
				}
			})
		}
	}
}

func TestWebFetchContinuationReadableAfterSessionResume(t *testing.T) {
	workspace := t.TempDir()
	cfg := &config.Config{Workspace: workspace, SessionDir: filepath.Join(t.TempDir(), "sessions")}
	sessionDir := filepath.Join(cfg.SessionDir, "current")
	r := tools.NewResources("current", sessionDir)
	defer r.Close()
	ctx := r.Context(nil, workspace, cfg.Sandbox())
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("preview ", 50)+"\ntruncated正文\nlast line\n")
	}))
	defer proxy.Close()
	ctx.Sandbox.NetworkProxy = proxy.URL
	ctx.HostNetworkAllowed = true
	ctx.Args = map[string]any{"url": "http://public.invalid/article", "max_chars": 256}
	out, err := tools.NewWebFetchTool().Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, hint, ok := strings.Cut(out, "[incomplete; use Read on ")
	if !ok {
		t.Fatalf("missing continuation: %s", out)
	}
	path, err := strconv.Unquote(strings.TrimSuffix(hint, " to continue]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, resume := range []bool{false, true} {
		if resume {
			_ = r.Close()
			r = tools.NewResources("current", sessionDir)
			defer r.Close()
			ctx = r.Context(nil, workspace, buildSandbox(cfg, workspace))
		}
		ctx.Args = map[string]any{"file_path": path, "offset": 2, "limit": 1}
		got, err := tools.NewReadTool().Run(ctx)
		if err != nil || !strings.Contains(got, "2\ttruncated正文") || !strings.Contains(got, "next_offset=3") {
			t.Fatalf("resume=%v: %q, %v", resume, got, err)
		}
	}
}
