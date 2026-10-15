package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccdp/internal/executable"
)

func TestRgRespectsSearchPolicyAndNativeUnicode(t *testing.T) {
	if executable.FindTrusted("rg", os.Getenv("PATH")) == "" {
		t.Skip("trusted rg unavailable")
	}
	root := t.TempDir()
	for path, body := range map[string]string{".gitignore": "ignored.txt\n", "ignored.txt": "needle\n", "allowed.txt": "before\nneedle needle\nafter\n中文\n", "secret.txt": "needle secret\n", "binary.dat": "needle\x00binary\n", "link_target.txt": "noise\n", ".hidden.txt": "needle\n", "node_modules/x": "needle\n", "nested/.gitignore": "skip.txt\n", "nested/skip.txt": "needle\n"} {
		reviewRgWrite(t, root, path, body)
	}
	ctx := rgCtx(root)
	ctx.Sandbox.AddDisallowedDir(filepath.Join(root, "secret.txt"))
	for _, req := range []*rgRequest{
		{pattern: "needle", base: root, outputMode: rgModeContent, before: 1, after: 1},
		{pattern: `\b\w+\b`, base: root, outputMode: rgModeContent},
		{pattern: `\p{L}+`, base: root, outputMode: rgModeContent},
		{pattern: "needle", base: root, outputMode: rgModeFiles},
		{pattern: "needle", base: root, outputMode: rgModeCount},
	} {
		got, gt, err := ripgrepSearch(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		output := formatRgResult(got, req, gt)
		for _, denied := range []string{"secret.txt", "ignored.txt:", "node_modules", "nested/skip.txt", "binary.dat"} {
			if strings.Contains(output, denied) {
				t.Errorf("search leaked %s: %s", denied, output)
			}
		}
		if !strings.Contains(output, "allowed.txt") {
			t.Errorf("missing allowed result: %s", output)
		}
		if req.pattern == `\b\w+\b` && !strings.Contains(output, "中文") {
			t.Errorf("rg Unicode word matching lost: %s", output)
		}

	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ctx.Context = cancelled
	if _, _, err := ripgrepSearch(ctx, &rgRequest{pattern: "needle", base: root}); err == nil {
		t.Fatal("cancelled native search succeeded")
	}
}

func TestRipgrepLimitsCountsAndArgumentBoundaries(t *testing.T) {
	if executable.FindTrusted("rg", os.Getenv("PATH")) == "" {
		t.Skip("trusted rg unavailable")
	}
	root := t.TempDir()
	reviewRgWrite(t, root, "-flags\nfile.txt", strings.Repeat("-needle\n", 600))
	reviewRgWrite(t, root, "b.txt", "-needle\n")
	for _, mode := range []string{rgModeFiles, rgModeCount, rgModeContent} {
		req := &rgRequest{pattern: "-needle", base: root, outputMode: mode, maxLines: 1}
		got, truncated, err := ripgrepSearch(rgCtx(root), req)
		if err != nil {
			t.Fatal(err)
		}
		if !truncated {
			t.Fatalf("missing result truncation for %s", mode)
		}
		if mode == rgModeCount && got[0].count != 600 {
			t.Fatalf("partial file count: %+v", got)
		}
	}
}

func TestRgMissingDependencyFailsWithoutFallback(t *testing.T) {
	root := t.TempDir()
	reviewRgWrite(t, root, "visible.txt", "needle")
	t.Setenv("PATH", t.TempDir())
	if executable.FindTrusted("rg", os.Getenv("PATH")) != "" {
		t.Skip("rg installed in system fallback PATH")
	}
	ctx := rgCtx(root)
	ctx.Args = map[string]any{"pattern": "needle"}
	output, err := NewRgTool().Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "ripgrep (rg) is required") || output != "" {
		t.Fatalf("missing native dependency silently fell back: %q %v", output, err)
	}
}

func TestRgNativeOptionsAndEmptyDirectoryValidation(t *testing.T) {
	root := t.TempDir()
	ctx := rgCtx(root)
	ctx.Args = map[string]any{"pattern": "["}
	if _, err := NewRgTool().Run(ctx); err == nil {
		t.Fatal("invalid regex accepted in empty directory")
	}
	reviewRgWrite(t, root, "text.txt", "a.b\naxb\nFIRST\nSECOND\n")
	for _, tc := range []struct {
		args         map[string]any
		want, absent string
	}{
		{map[string]any{"pattern": "a.b", "literal": true}, "a.b", "axb"},
		{map[string]any{"pattern": "first.*second", "ignore_case": true, "multiline": true}, "text.txt:3:FIRST", "0 match(es)"},
	} {
		ctx.Args = tc.args
		output, err := NewRgTool().Run(ctx)
		if err != nil || !strings.Contains(output, tc.want) || strings.Contains(output, tc.absent) {
			t.Fatalf("native options %v: %q %v", tc.args, output, err)
		}
	}
}
