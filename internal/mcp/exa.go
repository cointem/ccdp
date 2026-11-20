package mcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"ccdp/internal/protocol"
	"ccdp/internal/tools"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const ExaServer = "exa"
const exaSearchTool = "web_search_advanced_exa"
const exaEndpoint = "https://mcp.exa.ai/mcp?tools=" + exaSearchTool

// Bundled tool definition for offline discovery of the default server.
// A request checks the live catalog before calling the tool.
//
//go:embed exa_tool.json
var exaManifest []byte

var shippedExa = sync.OnceValue(func() ServerConfig {
	var def ToolDef
	if err := json.Unmarshal(exaManifest, &def); err != nil || def.Name != exaSearchTool {
		panic("invalid bundled Exa MCP manifest")
	}
	return ServerConfig{Transport: "http", BaseURL: exaEndpoint, NetworkAuthorized: true, DefaultInstall: true, OnDemandTools: []ToolDef{def}}
})

func DefaultExaServer() ServerConfig { return shippedExa().Clone() }

func IsExaSearch(tool tools.Tool) bool {
	t, ok := tool.(*mcpTool)
	return ok && t.client.isHostedExaSearch(t.def.Name)
}

func (c *Client) isHostedExaSearch(name string) bool {
	return c.name == ExaServer && name == exaSearchTool && c.cfg.IsHostedExa()
}

func (cfg ServerConfig) IsHostedExa() bool {
	return cfg.Transport == "http" && cfg.BaseURL == exaEndpoint
}

func ExaToolName() string { return protocol.MCPToolName(ExaServer, exaSearchTool) }

// BindExaCredential applies the key from this invocation's settings, so saving
// a new key also takes effect in open sessions. Only the exact hosted service
// may consume it when opening a per-call HTTP session.
func BindExaCredential(ctx context.Context, tool tools.Tool, key string) context.Context {
	if !IsExaSearch(tool) {
		return ctx
	}
	headers := map[string]string{}
	if key != "" {
		headers["x-api-key"] = key
	}
	return context.WithValue(ctx, invocationHeadersKey{}, invocationHeaders{tool.(*mcpTool).client, headers})
}

var errExaQuota = errors.New("Exa quota or rate limit reached")

func exaQuotaResponse(reply *sdk.CallToolResult) bool {
	if limited, _ := reply.Meta["ai.exa/rateLimited"].(bool); limited {
		return true
	}
	for _, content := range reply.Content {
		if block, ok := content.(*sdk.TextContent); ok {
			msg := strings.TrimSpace(block.Text)
			return strings.HasPrefix(msg, "You've hit Exa's free MCP rate limit.") || reply.IsError &&
				(strings.HasPrefix(msg, exaSearchTool+" error (429):") || strings.HasPrefix(msg, exaSearchTool+" error (402):") && exaCreditLimit(msg))
		}
	}
	return false
}

func exaQuotaHTTPError(err error) bool {
	var status *httpStatusError
	return errors.As(err, &status) && (status.code == http.StatusTooManyRequests || status.code == http.StatusPaymentRequired && exaCreditLimit(status.body))
}

func exaCreditLimit(message string) bool {
	msg := strings.ToLower(message)
	return !strings.Contains(msg, "x402_") && !strings.Contains(msg, "mpp_") &&
		(strings.Contains(msg, "credits") || strings.Contains(msg, "insufficient credit") || strings.Contains(msg, "budget"))
}
