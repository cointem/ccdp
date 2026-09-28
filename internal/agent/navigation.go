package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"ccdp/internal/sandbox"
	"ccdp/internal/workspace"
)

func (a *Agent) semanticNavigate(ctx context.Context, cfg config.Config, sb *sandbox.Sandbox, op, path string, line, character int) (string, error) {
	if path == "" {
		return "", fmt.Errorf("semantic navigation requires path")
	}
	keys := []string{}
	for key := range cfg.LanguageServers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		server := cfg.LanguageServers[key]
		matches := false
		for _, ext := range server.Extensions {
			if strings.EqualFold(filepath.Ext(path), ext) {
				matches = true
			}
		}
		if !matches {
			continue
		}
		args := map[string]any{"command": formatExternalCommand(server.Argv), "language_server": key}
		if e := a.precheckCommand("Bash", args); e != nil {
			return "", e
		}
		if e := a.authorizeCommand(ctx, protocol.CommandID(nextRuntimeID("lsp")), "Bash", args); e != nil {
			return "", e
		}
		locations, e := workspace.LanguageQuery(ctx, cfg.Workspace, path, op, line, character, server, sb)
		if e != nil {
			return "", e
		}
		b, e := json.Marshal(map[string]any{"source": "lsp", "position_encoding": "utf-16", "locations": locations})
		return string(b), e
	}
	return "", fmt.Errorf("no trusted language server configured for %s; use Grep and Read", filepath.Ext(path))
}
