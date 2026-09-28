package config

import (
	"ccdp/internal/workspace"
	"testing"
)

func TestLanguageServerRequiresLanguageID(t *testing.T) {
	for _, id := range []string{"", " \t", "go"} {
		cfg := Default()
		cfg.LanguageServers = map[string]workspace.LanguageServer{"go": {Argv: []string{"gopls"}, Extensions: []string{".go"}, LanguageID: id}}
		err := cfg.Validate()
		if (id == "go") != (err == nil) {
			t.Fatalf("language_id=%q: %v", id, err)
		}
	}
}
