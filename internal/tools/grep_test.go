package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"ccdp/internal/sandbox"
)

// grepFixture lays out a tree that exercises every filter the search backends
// claim to honor: ignored files, dependency/build noise, hidden files and a
// binary blob.
func grepFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "generated.txt\nbuild/\n")
	write("main.go", "package main\n\n// Needle here\nfunc main() {}\n\n// another needle\n")
	write("src/app.ts", "export const needle = 1\n")
	write("node_modules/pkg/index.js", "module.exports = 'needle'\n")
	write("generated.txt", "needle in an ignored file\n")
	write("build/out.go", "package build // needle\n")
	write(".hidden/notes.md", "# needle notes\n")
	write("data.bin", "needle\x00binary\n")
	write("types.go", "package main\n\ntype Needle struct {\n\tName string\n}\n")
	return dir
}

func grepCtx(dir string) *Context {
	return &Context{
		Context:    context.Background(),
		WorkingDir: dir,
		Sandbox:    sandbox.New(dir),
		Args:       map[string]any{},
	}
}

// resultLocations renders backend output as "file:line" pairs so the two engines
// can be compared without depending on their text formatting.
func resultLocations(lines []grepLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if !l.match {
			continue
		}
		out = append(out, l.file+":"+itoa(l.line))
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestInProcessSearchSkipsIgnoredAndNoise(t *testing.T) {
	dir := grepFixture(t)
	ctx := grepCtx(dir)
	lines, _, err := inProcessSearch(ctx, &grepRequest{pattern: "needle", base: dir, ignoreCase: true})
	if err != nil {
		t.Fatalf("inProcessSearch: %v", err)
	}
	got := strings.Join(resultLocations(lines), " ")
	for _, want := range []string{"main.go:3", "main.go:6", "src/app.ts:1", ".hidden/notes.md:1", "types.go:3"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %q", want, got)
		}
	}
	for _, unwanted := range []string{"node_modules", "generated.txt", "build/out.go", "data.bin"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s should have been filtered out, got %q", unwanted, got)
		}
	}
}

func TestSearchBackendsAgree(t *testing.T) {
	if ripgrepBinary() == "" {
		t.Skip("ripgrep not installed")
	}
	dir := grepFixture(t)
	ctx := grepCtx(dir)

	cases := []struct {
		name string
		req  grepRequest
	}{
		{"plain", grepRequest{pattern: "needle"}},
		{"case-insensitive", grepRequest{pattern: "NEEDLE", ignoreCase: true}},
		{"context", grepRequest{pattern: "needle", ignoreCase: true, before: 1, after: 1}},
		{"glob", grepRequest{pattern: "needle", ignoreCase: true, globs: []string{"*.go"}}},
		{"negative-glob", grepRequest{pattern: "needle", ignoreCase: true, globs: []string{"!*.md"}}},
		{"type", grepRequest{pattern: "needle", ignoreCase: true, types: []string{"ts"}}},
		{"only-match", grepRequest{pattern: "n\\w+le", ignoreCase: true, onlyMatch: true}},
		{"multiline", grepRequest{pattern: `struct \{[\s\S]*?Name`, multiline: true}},
		{"paginated", grepRequest{pattern: "needle", ignoreCase: true, maxLines: 2, offset: 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.base = dir
			want, _, err := ripgrepSearch(ctx, &req)
			if err != nil {
				t.Fatalf("ripgrepSearch: %v", err)
			}
			got, _, err := inProcessSearch(ctx, &req)
			if err != nil {
				t.Fatalf("inProcessSearch: %v", err)
			}
			wantLocs := strings.Join(resultLocations(want), "\n")
			gotLocs := strings.Join(resultLocations(got), "\n")
			if wantLocs != gotLocs {
				t.Errorf("match locations differ\nripgrep:\n%s\nin-process:\n%s", wantLocs, gotLocs)
			}
			if len(want) != len(got) {
				t.Errorf("line counts differ: ripgrep=%d in-process=%d", len(want), len(got))
			}
		})
	}
}

func TestGrepContextRendering(t *testing.T) {
	dir := grepFixture(t)
	ctx := grepCtx(dir)
	req := &grepRequest{pattern: "package main", base: dir, before: 1, after: 1}
	lines, _, err := inProcessSearch(ctx, req)
	if err != nil {
		t.Fatalf("inProcessSearch: %v", err)
	}
	out := formatGrepResult(lines, req, false)
	if !strings.Contains(out, "main.go-2-") {
		t.Errorf("expected a context line rendered with a dash separator:\n%s", out)
	}
	if !strings.Contains(out, "main.go:1:package main") {
		t.Errorf("expected the match line with a colon separator:\n%s", out)
	}
}

func TestGrepOutputModes(t *testing.T) {
	dir := grepFixture(t)
	ctx := grepCtx(dir)

	files := &grepRequest{pattern: "needle", base: dir, ignoreCase: true, outputMode: grepModeFiles}
	lines, _, err := inProcessSearch(ctx, files)
	if err != nil {
		t.Fatalf("inProcessSearch: %v", err)
	}
	out := formatGrepResult(lines, files, false)
	if !strings.Contains(out, "src/app.ts") || strings.Contains(out, ":1:") {
		t.Errorf("files_with_matches should list bare paths:\n%s", out)
	}

	count := &grepRequest{pattern: "needle", base: dir, ignoreCase: true, outputMode: grepModeCount}
	lines, _, err = inProcessSearch(ctx, count)
	if err != nil {
		t.Fatalf("inProcessSearch: %v", err)
	}
	out = formatGrepResult(lines, count, false)
	if !strings.Contains(out, "main.go:2") {
		t.Errorf("count mode should report 2 matches in main.go:\n%s", out)
	}
}

func TestGrepPaginationAndTruncationNote(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		b.WriteString("match line " + itoa(i) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := grepCtx(dir)

	req := &grepRequest{pattern: "match line", base: dir, maxLines: 3}
	lines, truncated, err := inProcessSearch(ctx, req)
	if err != nil {
		t.Fatalf("inProcessSearch: %v", err)
	}
	if !truncated {
		t.Error("expected the backend to report truncation")
	}
	out := formatGrepResult(lines, req, truncated)
	if n := strings.Count(out, "match line"); n != 3 {
		t.Errorf("expected 3 result lines, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "more result(s) available") {
		t.Errorf("expected a truncation note:\n%s", out)
	}

	page := &grepRequest{pattern: "match line", base: dir, maxLines: 3, offset: 3}
	lines, _, err = inProcessSearch(ctx, page)
	if err != nil {
		t.Fatalf("inProcessSearch: %v", err)
	}
	out = formatGrepResult(lines, page, false)
	if !strings.Contains(out, "match line 4") || strings.Contains(out, "match line 1\n") {
		t.Errorf("offset should skip the first page:\n%s", out)
	}
}

func TestGrepToolRun(t *testing.T) {
	dir := grepFixture(t)
	tool := NewGrepTool()

	run := func(args map[string]any) string {
		t.Helper()
		ctx := grepCtx(dir)
		ctx.Args = args
		out, err := tool.Run(ctx)
		if err != nil {
			t.Fatalf("Grep.Run(%v): %v", args, err)
		}
		return out
	}

	out := run(map[string]any{"pattern": "needle", "-i": true, "output_mode": "files_with_matches"})
	if !strings.Contains(out, "src/app.ts") {
		t.Errorf("expected src/app.ts in files output:\n%s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("dependency tree leaked into results:\n%s", out)
	}

	out = run(map[string]any{"pattern": "needle", "-i": true, "type": "go"})
	if strings.Contains(out, "app.ts") {
		t.Errorf("type filter did not restrict to Go files:\n%s", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("expected main.go for type go:\n%s", out)
	}

	out = run(map[string]any{"pattern": "needle", "-i": true, "-C": 1})
	if !strings.Contains(out, "--") && strings.Count(out, "main.go") > 1 {
		t.Errorf("expected a group separator between non-contiguous context blocks:\n%s", out)
	}

	if _, err := tool.Run(&Context{Context: context.Background(), WorkingDir: dir, Sandbox: sandbox.New(dir), Args: map[string]any{"pattern": "needle", "output_mode": "bogus"}}); err == nil {
		t.Error("expected an error for an unknown output_mode")
	}
	if _, err := tool.Run(&Context{Context: context.Background(), WorkingDir: dir, Sandbox: sandbox.New(dir), Args: map[string]any{"pattern": "(["}}); err == nil {
		t.Error("expected an error for a malformed regex")
	}
}

func TestIgnoreRules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("# comment\ndist/\n*.log\n!keep.log\nsrc/secret.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ir := newIgnoreRules()
	if err := ir.loadDir(dir, ""); err != nil {
		t.Fatalf("loadDir: %v", err)
	}
	cases := []struct {
		path string
		dir  bool
		want bool
	}{
		{"dist", true, true},
		{"dist/app.js", false, true},
		{"a/b/debug.log", false, true},
		{"keep.log", false, false},
		{"src/secret.go", false, true},
		{"src/other.go", false, false},
		{"distribution/x.go", false, false},
	}
	for _, tc := range cases {
		if got := ir.ignored(tc.path, tc.dir); got != tc.want {
			t.Errorf("ignored(%q, dir=%v) = %v, want %v", tc.path, tc.dir, got, tc.want)
		}
	}
}

func TestTranslateFileGlob(t *testing.T) {
	cases := []struct {
		glob string
		path string
		want bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "src/main.go", true},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "other/main.go", false},
		{"**/*.ts", "a/b/c.ts", true},
		{"*.{go,ts}", "a.ts", true},
		{"*.{go,ts}", "a.md", false},
		{"test_?.go", "test_a.go", true},
		{"test_?.go", "test_ab.go", false},
	}
	for _, tc := range cases {
		re := regexp.MustCompile(translateFileGlob(tc.glob))
		name := tc.path
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		got := re.MatchString(tc.path) || re.MatchString(name)
		if got != tc.want {
			t.Errorf("glob %q vs %q = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

func TestGrepFilePaginationUsesCompleteCounts(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"a.txt": strings.Repeat("hit\n", 600), "b.txt": "hit\nother\n", "c.txt": "hit\nhit\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	backends := map[string]func(*Context, *grepRequest) ([]grepLine, bool, error){"in_process": inProcessSearch}
	if ripgrepBinary() != "" {
		backends["ripgrep"] = ripgrepSearch
	}
	for name, search := range backends {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []string{grepModeCount, grepModeFiles} {
				for _, offset := range []int{0, 1, 2, 3} {
					req := &grepRequest{pattern: "hit", base: dir, outputMode: mode, maxLines: 2, offset: offset, before: 1, after: 1}
					lines, truncated, err := search(grepCtx(dir), req)
					if err != nil {
						t.Fatal(err)
					}
					out := formatGrepResult(lines, req, truncated)
					entries := []string{"a.txt", "b.txt", "c.txt"}
					if mode == grepModeCount {
						entries = []string{"a.txt:600", "b.txt:1", "c.txt:2"}
					}
					for i, entry := range entries {
						want := i >= offset && i < offset+2
						if strings.Contains(out, entry+"\n") != want {
							t.Errorf("mode=%s offset=%d entry=%s want=%v: %s", mode, offset, entry, want, out)
						}
					}
					if strings.Contains(out, "more result(s)") != (offset == 0) {
						t.Errorf("wrong truncation note: %s", out)
					}
				}
			}
			// A bounded process capture must not turn a prefix into a file count.
			ctx := grepCtx(dir)
			ctx.OutputLimit = 256
			req := &grepRequest{pattern: "hit", base: dir, outputMode: grepModeCount, maxLines: 2}
			lines, truncated, err := search(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			out := formatGrepResult(lines, req, truncated)
			if !strings.Contains(out, "a.txt:600\n") || !strings.Contains(out, "b.txt:1\n") {
				t.Fatalf("partial counts: %s", out)
			}
		})
	}
}
