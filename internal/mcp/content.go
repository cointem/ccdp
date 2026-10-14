package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Call invokes a tool on the server and returns its textual content. Image
// and resource content pieces are surfaced as placeholders so multi-modal
// output is never silently dropped.
func (c *Client) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	params := map[string]any{"name": name, "arguments": args}
	var resp struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
		IsError           bool              `json:"isError"`
	}

	// Bound the call when the caller gave us no deadline of its own.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	if err := c.requestJSON(ctx, "tools/call", params, &resp); err != nil {
		return "", err
	}
	var sb strings.Builder
	structured := len(resp.StructuredContent) > 0
	for _, raw := range resp.Content {
		var piece struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &piece); err != nil {
			return "", err
		}
		if piece.Type == "text" {
			sb.WriteString(piece.Text)
			sb.WriteByte('\n')
		} else {
			structured = true
		}
	}
	if structured {
		data, err := json.Marshal(resp)
		if err != nil {
			return "", err
		}
		if resp.IsError {
			return string(data), errors.New("MCP tool reported an error")
		}
		return string(data), nil
	}

	out := strings.TrimRight(sb.String(), "\n")
	if resp.IsError {
		if out == "" {
			out = "tool reported an error"
		}
		return out, errors.New(out)
	}
	return out, nil
}

// Resources lists the resources advertised by the server (resources/list).
func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var list struct {
		Resources []Resource `json:"resources"`
	}
	if err := c.requestJSON(ctx, "resources/list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	return list.Resources, nil
}

// ReadResource reads a resource by URI (resources/read). Text contents are
// returned verbatim; binary (blob) contents are surfaced as placeholders.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	var resp struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"contents"`
	}
	if err := c.requestJSON(ctx, "resources/read", map[string]any{"uri": uri}, &resp); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, pc := range resp.Contents {
		if pc.Text != "" {
			sb.WriteString(pc.Text)
			sb.WriteString("\n")
			continue
		}
		if pc.Blob != "" {
			fmt.Fprintf(&sb, "[binary resource %s (%s): %d bytes of base64 data]\n",
				pc.URI, pc.MimeType, len(pc.Blob))
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// Prompts lists the prompts advertised by the server (prompts/list).
func (c *Client) Prompts(ctx context.Context) ([]Prompt, error) {
	var list struct {
		Prompts []Prompt `json:"prompts"`
	}
	if err := c.requestJSON(ctx, "prompts/list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	return list.Prompts, nil
}

// GetPrompt retrieves a prompt's rendered messages (prompts/get) with
// template arguments.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	var resp struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	if err := c.requestJSON(ctx, "prompts/get", params, &resp); err != nil {
		return "", err
	}
	var sb strings.Builder
	if resp.Description != "" {
		fmt.Fprintf(&sb, "[%s]\n", resp.Description)
	}
	for _, m := range resp.Messages {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content.Text)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// Resource is one MCP resource (resources/list).
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// Prompt is one MCP prompt template (prompts/list).
type Prompt struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
