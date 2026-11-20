package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"time"

	"ccdp/internal/netguard"
)

const maxHTTPBytes = 10 * 1024 * 1024 // 10 MiB cap on any fetched body

// ---------- WebFetch ----------

// WebFetchTool retrieves a web page and converts it to plain text.
type WebFetchTool struct{}

// NewWebFetchTool creates the web fetch tool.
func NewWebFetchTool() *WebFetchTool { return &WebFetchTool{} }

func (t *WebFetchTool) Name() string { return "WebFetch" }

func (t *WebFetchTool) Description() string {
	return `Read HTML or text from a known public HTTP(S) URL. To find URLs, use ToolSearch to discover a web search MCP. PDF and other binary formats are unsupported. Preview defaults to 8000 characters; use ReadOutput for the saved full text. Requires outbound network access.`
}

func (t *WebFetchTool) Parameters() map[string]any {
	return WithCapabilityRequest(map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The http(s) URL to fetch.",
			},
			"max_chars": map[string]any{"default": 8000,
				"type":        "integer",
				"minimum":     256,
				"maximum":     40000,
				"description": "Optional character cap for the returned text (default 8000).",
			},
		},
		"required": []string{"url"},
	})
}

func (t *WebFetchTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	return executeText(ctx, args, func(ctx *Context) (string, error) {
		if err := ctx.checkResources(); err != nil {
			return "", err
		}
		if !ctx.HostNetworkAllowed {
			return "", errors.New("WebFetch: outbound network access is not authorized for this call")
		}
		raw := strings.TrimSpace(StringArg(ctx.Args, "url", ""))
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return "", fmt.Errorf("WebFetch: invalid URL %q (only http/https)", raw)
		}
		if u.Host == "" {
			return "", fmt.Errorf("WebFetch: URL %q has no host", raw)
		}

		maxChars, err := IntArgChecked(ctx.Args, "max_chars", 8000)
		if err != nil {
			return "", fmt.Errorf("WebFetch: %w", err)
		}
		if maxChars < 256 || maxChars > 40000 {
			return "", fmt.Errorf("WebFetch: max_chars must be 256..40000")
		}

		body, finalURL, contentType, status, err := httpGetDetails(ctx.Context, u.String(), webProxy(ctx))
		if err != nil {
			return "", fmt.Errorf("WebFetch: %w", err)
		}
		text := string(body)
		media, _, _ := mime.ParseMediaType(contentType)
		switch media {
		case "text/html", "application/xhtml+xml":
			text = htmlToText(text)
		case "", "text/plain", "text/markdown", "application/json", "application/xml", "text/xml":
		default:
			if !strings.HasPrefix(media, "text/") {
				return "", fmt.Errorf("WebFetch: unsupported content type %s", media)
			}
		}
		if strings.TrimSpace(text) == "" {
			return fmt.Sprintf("URL: %s\nHTTP %d: no readable text", finalURL, status), nil
		}
		header := fmt.Sprintf("URL: %s\nHTTP %d; content-type: %s\n\n", finalURL, status, contentType)
		runes := []rune(text)
		budget := min(50<<10, ctx.outputLimit()) - len(header) - 300
		if budget < 0 {
			return "", fmt.Errorf("WebFetch: output budget too small")
		}
		if len(runes) > maxChars || len(text) > budget {
			ctx.dataResult.FullText = header + text
			text = string(runes[:min(len(runes), maxChars)])
			text = truncateUTF8(text, budget)
		}
		return header + text, nil
	})
}

var reTag = regexp.MustCompile(`<[^>]+>`)

// errBlockedPrivate is returned when a request targets a loopback, private or
// link-local address: WebFetch must not be usable to reach cloud
// metadata endpoints, localhost services or the internal network (SSRF).
var errBlockedPrivate = errors.New("blocked: request to internal/private address")

// blockedHostnames are names that refer to internal services regardless of
// what they resolve to.
var blockedHostnames = map[string]bool{
	"localhost":                true,
	"metadata":                 true,
	"metadata.google.internal": true,
	"instance-data":            true,
}

// validateExternalURL rejects URLs that point at loopback, private or
// link-local addresses (or unrouteable internal hostnames).
func validateExternalURL(ctx context.Context, raw string) error {
	return validateExternalURLWithLookup(ctx, raw, resolveHostIPs)
}

// Lookup is explicit so policy tests do not depend on public DNS or host proxy
// configuration. The production entry point always uses bounded DNS resolution.
func validateExternalURLWithLookup(ctx context.Context, raw string, lookup func(context.Context, string) ([]net.IP, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errBlockedPrivate
	}
	host := u.Hostname()
	if host == "" || blockedHostnames[strings.ToLower(host)] {
		return errBlockedPrivate
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return errBlockedPrivate
		}
	}
	return nil
}

// resolveHostIPs returns the IPs a URL host refers to: IP literals directly,
// DNS names via a bounded lookup. An unresolvable name yields nothing — the
// request itself will fail with a DNS error anyway.
func resolveHostIPs(ctx context.Context, host string) ([]net.IP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// isPrivateIP reports whether ip is unspecified (0.0.0.0/::), loopback
// (127/8, ::1), private (10/8, 172.16/12, 192.168/16, fc00::/7) or
// link-local (169.254/16, fe80::/10) — everything an outbound tool fetch
// must not reach.
func isPrivateIP(ip net.IP) bool {
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

func webProxy(ctx *Context) string {
	if ctx.Sandbox == nil {
		return ""
	}
	return ctx.Sandbox.Snapshot().NetworkProxy
}

func httpGetDetails(ctx context.Context, u string, proxies ...string) (body []byte, finalURL, contentType string, status int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	proxy := ""
	if len(proxies) > 0 {
		proxy = proxies[0]
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	body, finalURL, contentType, status, err = httpGetOnce(ctx, u, proxy)
	if ctx.Err() == nil && netguard.Interrupted(err) {
		body, finalURL, contentType, status, err = httpGetOnce(ctx, u, proxy)
		if err != nil && netguard.Interrupted(err) && ctx.Err() == nil {
			err = fmt.Errorf("could not read a complete response after two GET attempts; check the source URL and network route: %w", err)
		}
	}
	return
}

func httpGetOnce(ctx context.Context, u, proxy string) (body []byte, finalURL, contentType string, status int, err error) {
	validate := validateExternalURL
	if proxy != "" {
		validate = func(ctx context.Context, raw string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			target, err := url.Parse(raw)
			if err != nil {
				return err
			}
			return netguard.ValidateTarget(target)
		}
	}
	if err := validate(ctx, u); err != nil {
		return nil, "", "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", "", 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ccdp/0.1)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	transport, err := netguard.RoutedTransport(proxy)
	if err != nil {
		return nil, "", "", 0, err
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return validate(req.Context(), req.URL.String())
	}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", "", 0, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxHTTPBytes+1))
	if err != nil {
		return nil, "", "", 0, err
	}
	if len(body) > maxHTTPBytes {
		return nil, "", "", 0, fmt.Errorf("HTTP response exceeds 10 MiB")
	}
	return body, resp.Request.URL.String(), resp.Header.Get("Content-Type"), resp.StatusCode, nil
}

// htmlToText strips markup into readable plain text.
func htmlToText(html string) string {
	// Drop script/style/head/noscript blocks first. RE2 (Go) has no
	// backreferences, so match each block tag explicitly.
	for _, tag := range []string{"script", "style", "head", "noscript"} {
		re := regexp.MustCompile(`(?is)<` + tag + `[^>]*>.*?</` + tag + `>`)
		html = re.ReplaceAllString(html, " ")
	}

	html = regexp.MustCompile(`(?is)<a\s[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`).ReplaceAllString(html, "[$2]($1)")
	html = regexp.MustCompile(`(?i)<pre[^>]*>`).ReplaceAllString(html, "\n```\n")
	html = regexp.MustCompile(`(?i)</pre>`).ReplaceAllString(html, "\n```\n")
	html = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(html, "\n")
	// Block-level elements become line breaks.
	reBreak := regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|tr|br|section|article|pre|table)>`)
	html = reBreak.ReplaceAllString(html, "\n")

	html = reTag.ReplaceAllString(html, " ")
	text := htmlUnescape(html)

	var sb strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// htmlUnescape decodes common HTML entities (no external dependency).
func htmlUnescape(s string) string {
	repl := []struct {
		from string
		to   string
	}{
		{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"},
		{"&quot;", `"`}, {"&#39;", "'"}, {"&nbsp;", " "},
		{"&#x27;", "'"}, {"&#x2F;", "/"}, {"&hellip;", "…"},
		{"&mdash;", "—"}, {"&ndash;", "–"}, {"&copy;", "©"},
	}
	for _, r := range repl {
		s = strings.ReplaceAll(s, r.from, r.to)
	}
	return s
}
