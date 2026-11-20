package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	ctx := rgCtx(t, root)
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
		got, truncated, err := ripgrepSearch(rgCtx(t, root), req)
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
	ctx := rgCtx(t, root)
	ctx.Args = map[string]any{"pattern": "needle"}
	output, err := executeToolText(NewRgTool(), ctx)
	if err == nil || !strings.Contains(err.Error(), "ripgrep (rg) is required") || output != "" {
		t.Fatalf("missing native dependency silently fell back: %q %v", output, err)
	}
}

func TestRgFromFinderWithHomebrewInstallation(t *testing.T) {
	if runtime.GOOS != "darwin" || executable.File("/opt/homebrew/bin/rg") == "" && executable.File("/usr/local/bin/rg") == "" {
		t.Skip("requires macOS with Homebrew ripgrep")
	}
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	root := t.TempDir()
	reviewRgWrite(t, root, "fixture.go", "package lockservice\n")
	ctx := rgCtx(t, root)
	ctx.Args = map[string]any{"pattern": "lockservice"}
	got, err := executeToolText(NewRgTool(), ctx)
	if err != nil || !strings.Contains(got, "fixture.go:1:package lockservice") {
		t.Fatalf("Finder search: %q, %v", got, err)
	}
}

func TestRgFromDesktopBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS app bundle")
	}
	if os.Getenv("CCDP_TEST_BUNDLED_SEARCH") == "1" {
		t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
		if err := executable.UseDesktopBundle(); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(executable.DesktopBundle(), "Contents", "Resources", "bin", "rg")
		shellPath, err := exec.LookPath("rg")
		if shellPath != want || err != nil || executable.FindTrusted("rg", "/opt/homebrew/bin") != want {
			t.Fatalf("could not use the bundled rg for both native and shell searches: %q, %v", shellPath, err)
		}
		root := t.TempDir()
		reviewRgWrite(t, root, "fixture.go", "package lockservice\n")
		ctx := rgCtx(t, root)
		ctx.Args = map[string]any{"pattern": "lockservice"}
		got, err := executeToolText(NewRgTool(), ctx)
		if err != nil || !strings.Contains(got, "fixture.go:1:package lockservice") {
			t.Fatalf("bundled search: %q, %v", got, err)
		}
		return
	}
	// Run the real search adapter from the layout MyGo produces, independent
	// of Homebrew and without changing the test process's executable lookup.
	contents := filepath.Join(t.TempDir(), "ccdp.app", "Contents")
	host := filepath.Join(contents, "MacOS", "ccdp")
	rg := filepath.Join(contents, "Resources", "bin", "rg")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join("..", "..", "resources", "darwin-"+runtime.GOARCH, "bin", "rg")
	if executable.File(source) == "" {
		source = executable.FindTrusted("rg", os.Getenv("PATH"))
		if source == "" {
			t.Skip("build the desktop bundle or install ripgrep to exercise bundled search")
		}
	}
	for src, dst := range map[string]string{self: host, source: rg} {
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("/bin/cp", src, dst).CombinedOutput(); err != nil {
			t.Fatalf("copy bundle fixture: %v: %s", err, out)
		}
	}
	cmd := exec.Command(host, "-test.run=^TestRgFromDesktopBundle$")
	cmd.Env = append(os.Environ(), "CCDP_TEST_BUNDLED_SEARCH=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("desktop search: %v\n%s", err, out)
	}
}

func TestRgNativeOptionsAndEmptyDirectoryValidation(t *testing.T) {
	root := t.TempDir()
	ctx := rgCtx(t, root)
	ctx.Args = map[string]any{"pattern": "["}
	if _, err := executeToolText(NewRgTool(), ctx); err == nil {
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
		output, err := executeToolText(NewRgTool(), ctx)
		if err != nil || !strings.Contains(output, tc.want) || strings.Contains(output, tc.absent) {
			t.Fatalf("native options %v: %q %v", tc.args, output, err)
		}
	}
}
