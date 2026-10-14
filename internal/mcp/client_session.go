package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (c *Client) sdkClient() *sdk.Client {
	return sdk.NewClient(&sdk.Implementation{Name: "ccdp", Version: "0.2.0"}, &sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}, ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) { c.scheduleRefresh() }})
}

// requestJSON adapts existing application DTOs; wire framing, negotiation,
// session headers, notifications and request completion belong to the SDK.
func (c *Client) requestJSON(ctx context.Context, method string, params map[string]any, dst any) error {
	c.mu.Lock()
	session := c.session
	lifetime := c.sessionCtx
	c.mu.Unlock()
	if session == nil || c.isClosed() {
		return fmt.Errorf("mcp %s: session unavailable", c.name)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	// The SDK cancels an unfinished streaming body when this request returns.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(lifetime, cancel)
	defer stop()
	if err := lifetime.Err(); err != nil {
		return err
	}
	var result any
	var err error
	switch method {
	case "tools/list":
		var items []*sdk.Tool
		for item, e := range session.Tools(ctx, nil) {
			if e != nil {
				return e
			}
			items = append(items, item)
		}
		result = struct {
			Tools []*sdk.Tool `json:"tools"`
		}{items}
	case "tools/call":
		name, _ := params["name"].(string)
		result, err = session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: params["arguments"]})
	case "resources/list":
		var items []*sdk.Resource
		for item, e := range session.Resources(ctx, nil) {
			if e != nil {
				return e
			}
			items = append(items, item)
		}
		result = struct {
			Resources []*sdk.Resource `json:"resources"`
		}{items}
	case "resources/read":
		uri, _ := params["uri"].(string)
		result, err = session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	case "prompts/list":
		var items []*sdk.Prompt
		for item, e := range session.Prompts(ctx, nil) {
			if e != nil {
				return e
			}
			items = append(items, item)
		}
		result = struct {
			Prompts []*sdk.Prompt `json:"prompts"`
		}{items}
	case "prompts/get":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]string)
		result, err = session.GetPrompt(ctx, &sdk.GetPromptParams{Name: name, Arguments: args})
	default:
		return fmt.Errorf("mcp %s: unsupported method %s", c.name, method)
	}
	if err != nil || dst == nil {
		return err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}
