package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const maxHTTPBytes = 2 * 1024 * 1024 // 2 MiB cap on any fetched body

// ---------- WebFetch ----------

// WebFetchTool retrieves a web page and converts it to plain text.
type WebFetchTool struct{}

// NewWebFetchTool creates the web fetch tool.
func NewWebFetchTool() *WebFetchTool { return &WebFetchTool{} }

func (t *WebFetchTool) Name() string { return "WebFetch" }

func (t *WebFetchTool) Description() string {
	return `Fetch a URL and return its content as readable plain text. Use this to
read documentation, error pages or any public web resource. HTML is stripped
to text. Content is capped (default 8000 chars). Network access requires
approval in default permission mode.`
}

func (t *WebFetchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The http(s) URL to fetch.",
			},
			"max_chars": map[string]any{
				"type":        "integer",
				"description": "Optional character cap for the returned text (default 8000).",
			},
		},
		"required": []string{"url"},
	}
}

func (t *WebFetchTool) Run(ctx *Context) (string, error) {
	raw := StringArg(ctx.Args, "url", "")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("WebFetch: invalid URL %q (only http/https)", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("WebFetch: URL %q has no host", raw)
	}

	maxChars := IntArg(ctx.Args, "max_chars", 8000)
	if maxChars < 256 {
		maxChars = 256
	}
	if maxChars > 40000 {
		maxChars = 40000
	}

	body, err := httpGet(ctx.Context, u.String())
	if err != nil {
		return "", fmt.Errorf("WebFetch: %w", err)
	}
	text := htmlToText(string(body))
	text = strings.Join(strings.Fields(text), " ") // collapse whitespace
	if len(text) > maxChars {
		text = text[:maxChars] + fmt.Sprintf(" …[truncated at %d chars]", maxChars)
	}
	if text == "" {
		return "WebFetch: page returned no readable text", nil
	}
	return text, nil
}

// ---------- WebSearch ----------

// WebSearchTool performs a best-effort web search through DuckDuckGo's HTML
// endpoint (no API key required). Results are title + URL + snippet.
type WebSearchTool struct{}

// NewWebSearchTool creates the web search tool.
func NewWebSearchTool() *WebSearchTool { return &WebSearchTool{} }

func (t *WebSearchTool) Name() string { return "WebSearch" }

func (t *WebSearchTool) Description() string {
	return `Search the web for a query and return the top results with titles, URLs
and snippets. Useful for up-to-date information, library versions, error
messages and documentation lookups. Backed by DuckDuckGo's public HTML
endpoint; results may be limited. Network access requires approval.`
}

func (t *WebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query.",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Optional maximum number of results (default 5, max 10).",
			},
		},
		"required": []string{"query"},
	}
}

func (t *WebSearchTool) Run(ctx *Context) (string, error) {
	q := StringArg(ctx.Args, "query", "")
	if strings.TrimSpace(q) == "" {
		return "", fmt.Errorf("WebSearch: empty query")
	}
	max := IntArg(ctx.Args, "max_results", 5)
	if max < 1 {
		max = 1
	}
	if max > 10 {
		max = 10
	}

	endpoint := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(q)
	body, err := httpGet(ctx.Context, endpoint)
	if err != nil {
		return "", fmt.Errorf("WebSearch: %w", err)
	}
	results := parseDuckDuckGo(string(body), max)
	if len(results) == 0 {
		return fmt.Sprintf("WebSearch: no results for %q", q), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d result(s) for %q:\n", len(results), q)
	for i, r := range results {
		fmt.Fprintf(&sb, "\n%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return sb.String(), nil
}

// searchResult is one parsed search hit.
type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

var (
	reResult = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>.*?<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	reTag    = regexp.MustCompile(`<[^>]+>`)
)

// parseDuckDuckGo extracts search results from the DDG HTML page.
func parseDuckDuckGo(html string, max int) []searchResult {
	var out []searchResult
	for _, m := range reResult.FindAllStringSubmatch(html, max) {
		if len(m) < 4 {
			continue
		}
		href := htmlUnescape(reTag.ReplaceAllString(m[1], ""))
		title := htmlUnescape(reTag.ReplaceAllString(m[2], ""))
		snippet := htmlUnescape(reTag.ReplaceAllString(m[3], ""))
		if href == "" || title == "" {
			continue
		}
		out = append(out, searchResult{Title: strings.TrimSpace(title), URL: href, Snippet: strings.TrimSpace(snippet)})
	}
	return out
}

// errBlockedPrivate is returned when a request targets a loopback, private or
// link-local address: WebFetch/WebSearch must not be usable to reach cloud
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
func validateExternalURL(raw string) error {
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
	for _, ip := range resolveHostIPs(host) {
		if isPrivateIP(ip) {
			return errBlockedPrivate
		}
	}
	return nil
}

// resolveHostIPs returns the IPs a URL host refers to: IP literals directly,
// DNS names via a bounded lookup. An unresolvable name yields nothing — the
// request itself will fail with a DNS error anyway.
func resolveHostIPs(host string) []net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips
}

// isPrivateIP reports whether ip is unspecified (0.0.0.0/::), loopback
// (127/8, ::1), private (10/8, 172.16/12, 192.168/16, fc00::/7) or
// link-local (169.254/16, fe80::/10) — everything an outbound tool fetch
// must not reach.
func isPrivateIP(ip net.IP) bool {
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// checkExternalRedirect is the http.Client CheckRedirect hook: every hop of a
// redirect chain is re-validated, so a public URL can't bounce the fetch onto
// an internal address.
func checkExternalRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return validateExternalURL(req.URL.String())
}

// httpGet performs a GET with a timeout, an SSRF address check (initial URL
// and every redirect hop) and a browser-ish user agent.
func httpGet(ctx context.Context, u string) ([]byte, error) {
	if err := validateExternalURL(u); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ccdp/0.1)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	client := &http.Client{CheckRedirect: checkExternalRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBytes))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// htmlToText strips markup into readable plain text.
func htmlToText(html string) string {
	// Drop script/style/head/noscript blocks first. RE2 (Go) has no
	// backreferences, so match each block tag explicitly.
	for _, tag := range []string{"script", "style", "head", "noscript"} {
		re := regexp.MustCompile(`(?is)<` + tag + `[^>]*>.*?</` + tag + `>`)
		html = re.ReplaceAllString(html, " ")
	}

	// Block-level elements become line breaks.
	reBreak := regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|tr|br|section|article|pre|table)>`)
	html = reBreak.ReplaceAllString(html, "\n")

	html = reTag.ReplaceAllString(html, " ")
	text := htmlUnescape(html)

	var sb strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
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
