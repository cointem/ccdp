package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Extracted components must remain usable without constructing an Agent. The
// reducer additionally cannot acquire resources or synchronize workers.
func TestExtractedPackageBoundaries(t *testing.T) {
	for _, pkg := range []string{"session/replay", "session/archive", "session/journal", "sessionview", "tooling", "supervision"} {
		t.Run(pkg, func(t *testing.T) {
			dir := filepath.Join("..", filepath.FromSlash(pkg))
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				name := entry.Name()
				if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
					continue
				}
				parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
				if err != nil {
					t.Fatal(err)
				}
				for _, imp := range parsed.Imports {
					path, err := strconv.Unquote(imp.Path.Value)
					if err != nil {
						t.Fatal(err)
					}
					if path == "ccdp/internal/agent" || path == "ccdp/internal/tui" {
						t.Errorf("%s imports runtime/UI %s", name, path)
					}
					if pkg == "session/replay" && (path == "os" || path == "context" || path == "sync" || path == "net" || strings.HasPrefix(path, "net/") || strings.HasPrefix(path, "ccdp/internal/session/")) {
						t.Errorf("%s: reducer imports resource dependency %s", name, path)
					}
				}
			}
		})
	}
}
