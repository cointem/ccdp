package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the connection settings for the LLM provider.
type Config struct {
	APIModel string // concrete wire ID; Model remains the qualified registry identity
	BaseURL  string // e.g. https://api.openai.com/v1 or a compatible gateway
	APIKey   string
	Model    string
	// ContextWindow and MaxOutputTokens are operator/provider limits used by
	// request admission. Zero means the adapter does not publish that limit.
	ContextWindow   int
	MaxOutputTokens int
	Timeout         time.Duration // overall request timeout
	HTTPClient      *http.Client
	MaxRetries      int           // transient-error retries (default 3)
	RetryDelay      time.Duration // base backoff delay (default 500ms, doubles each retry)
	// OneAttempt disables the adapter's internal retry loop. Runtime callers
	// that journal one provider invocation per attempt use this mode so a
	// transient HTTP failure is returned to the owning loop instead of being
	// hidden inside a single Stream call. Direct clients retain the historical
	// MaxRetries default when OneAttempt is false.
	OneAttempt bool
	Debug      bool // print request summaries to stderr
	// Wire selects the provider wire format: "chat" (default; Chat Completions
	// /chat/completions) or "responses" (OpenAI Responses /responses).
	Wire string
}

// RetryableError marks a provider failure that may be retried by the owning
// runtime. The HTTP adapter uses this marker for transient transport/status
// failures. RetryAfter is an optional server hint; it is retained on the
// error so a journaled one-attempt caller can apply the same bounded delay
// that the direct Client loop would have used.
type RetryableError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *RetryableError) Error() string {
	if e == nil || e.Err == nil {
		return "llm: retryable provider error"
	}
	return e.Err.Error()
}

func (e *RetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// RetryAfter reports whether err is a transient provider failure and returns
// its optional server-provided delay. It follows wrapped errors so callers
// can add context without losing the retry classification.
func RetryAfter(err error) (time.Duration, bool) {
	var retryable *RetryableError
	if !errors.As(err, &retryable) || retryable == nil {
		return 0, false
	}
	return retryable.RetryAfter, true
}

// Provider is the model abstraction behind the plugin ModelRegistry (the pi
// ModelRegistry idea). Any provider can be registered dynamically; the built-in
// OpenAI-compatible client implements it, and third parties can drop in their
// own by matching this surface.
type Provider interface {
	// Name identifies the provider in the registry (usually the model name).
	Name() string
	// Stream performs a streaming completion; onDelta receives text fragments.
	Stream(ctx context.Context, req CompletionRequest, onDelta func(string)) (StreamResult, error)
}

// Client is a streaming Chat Completions client.
type Client struct {
	cfg        Config
	httpc      *http.Client
	baseURL    string
	maxRetries int
	retryDelay time.Duration
}

// NewClient validates the config and returns a client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: base_url is required")
	}
	baseURL := normalizeBaseURL(cfg.BaseURL)
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: cfg.Timeout}
	}
	maxRetries := cfg.MaxRetries
	if cfg.OneAttempt {
		maxRetries = 0
	} else if maxRetries <= 0 {
		maxRetries = 3
	}
	retryDelay := cfg.RetryDelay
	if retryDelay <= 0 {
		retryDelay = 500 * time.Millisecond
	}
	return &Client{cfg: cfg, httpc: httpc, baseURL: baseURL, maxRetries: maxRetries, retryDelay: retryDelay}, nil
}

// DebugLog prints a request summary to stderr when debug mode is enabled.
func (c *Client) DebugLog(format string, a ...any) {
	if c.cfg.Debug {
		fmt.Fprintf(os.Stderr, "llm: "+format+"\n", a...)
	}
}

// Name reports the configured model; it satisfies the Provider interface.
func (c *Client) Name() string { return c.cfg.Model }

// Endpoint reports the adapter's resolved base URL for diagnostics and
// request manifests. It never exposes the API key.
func (c *Client) Endpoint() string { return c.baseURL }

// Capabilities describes the OpenAI-compatible wire surface implemented by
// this adapter.  It is intentionally conservative about optional reasoning
// and usage extensions: the adapter accepts those fields when present, while
// the request admission contract only requires the core chat capabilities.
func (c *Client) Capabilities() Capabilities {
	return Capabilities{
		ToolCalling:     true,
		Images:          true,
		ContextWindow:   c.cfg.ContextWindow,
		MaxOutputTokens: c.cfg.MaxOutputTokens,
		Reasoning:       true,
		SystemRole:      true,
		Usage:           true,
	}
}

// StreamResult is the final outcome of a streaming call.
type StreamResult struct {
	CacheReported bool
	Text          string
	Reasoning     string // chain-of-thought deltas, when the provider sends them
	ToolCalls     []ToolCall
	FinishReason  string
	PromptTokens  int
	CompletionTok int
	CachedTokens  int // prompt tokens served from a provider cache (if reported)
}

// Stream performs a streaming chat completion and calls onDelta for each text
// fragment. The final aggregated result is returned. Transient failures (network
// errors, HTTP 429/5xx) are retried with exponential backoff up to MaxRetries.
func (c *Client) Stream(ctx context.Context, req CompletionRequest, onDelta func(string)) (StreamResult, error) {
	return c.stream(ctx, req, onDelta, nil)
}

// StreamWithReasoning is Stream but additionally delivers chain-of-thought
// deltas (OpenAI reasoning_content) through onReasoning.
func (c *Client) StreamWithReasoning(ctx context.Context, req CompletionRequest, onDelta func(string), onReasoning func(string)) (StreamResult, error) {
	return c.stream(ctx, req, onDelta, onReasoning)
}

func (c *Client) stream(ctx context.Context, req CompletionRequest, onDelta func(string), onReasoning func(string)) (StreamResult, error) {
	req.Stream = true
	body, err := c.marshalRequestBody(req)
	if err != nil {
		return StreamResult{}, fmt.Errorf("llm: marshal request: %w", err)
	}

	for attempt := 0; ; attempt++ {
		result, retryable, emitted, retryAfter, rerr := c.streamOnce(ctx, body, onDelta, onReasoning)
		if rerr == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// Retrying replays the stream from the start: if any delta already
		// reached the caller (UI + history), a retry would duplicate it.
		if !retryable || emitted || attempt >= c.maxRetries {
			return result, rerr
		}
		// A provider-provided Retry-After (rate limits) wins over backoff
		// (Claude's withRetry behavior); cap it to the request timeout.
		delay := c.retryDelay * time.Duration(1<<attempt)
		if retryAfter > 0 {
			delay = retryAfter
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// isResponses reports whether this client uses the OpenAI Responses wire
// (POST /responses + response.xxx SSE events) instead of Chat Completions.
func (c *Client) isResponses() bool {
	return strings.EqualFold(strings.TrimSpace(c.cfg.Wire), "responses")
}

// isAnthropic reports whether this client uses the Anthropic Messages wire
// (POST /v1/messages + anthropic-framed SSE events + x-api-key auth).
func (c *Client) isAnthropic() bool {
	return strings.EqualFold(strings.TrimSpace(c.cfg.Wire), "anthropic")
}

// normalizeBaseURL appends /v1 to a bare external host (no path) so a user
// writing "https://gateway.example" hits the OpenAI-compatible /v1 prefix for
// both the chat (/v1/chat/completions) and responses (/v1/responses) wires. It
// is idempotent and conservative: an explicit trailing /v1, /api, or any
// concrete path is honored unchanged, and loopback hosts (Ollama etc. that
// serve at the root) are left alone. Callers serving at a root path can always
// pin the exact endpoint by writing it out.
func normalizeBaseURL(raw string) string {
	b := strings.TrimRight(strings.TrimSpace(raw), "/")
	if i := strings.Index(b, "://"); i >= 0 {
		rest := b[i+3:]
		if strings.Contains(rest, "/") {
			return b // concrete path already present
		}
		host := rest
		if colon := strings.IndexByte(host, ':'); colon >= 0 {
			host = host[:colon]
		}
		if isLoopbackHost(host) {
			return b
		}
	}
	return b + "/v1"
}

func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// marshalRequestBody builds the wire-specific request payload.
func (c *Client) marshalRequestBody(req CompletionRequest) ([]byte, error) {
	if c.cfg.APIModel != "" {
		req.Model = c.cfg.APIModel
	}
	if c.isResponses() {
		return c.responsesBody(req)
	}
	if c.isAnthropic() {
		return c.anthropicBody(req)
	}
	return json.Marshal(req)
}

// postStream sends one streaming POST attempt and surfaces transient failures.
// On success it returns the open 200+SSE response (caller owns resp.Body); on
// any non-stream outcome it returns the classified error without a body handle.
func (c *Client) postStream(ctx context.Context, url string, body []byte) (resp *http.Response, retryable bool, retryAfter time.Duration, err error) {
	c.DebugLog("POST %s (payload %d bytes)", url, len(body))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, false, 0, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.cfg.APIKey != "" {
		if c.isAnthropic() {
			httpReq.Header.Set("x-api-key", c.cfg.APIKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
			if betas := anthropicBetaHeader(body); betas != "" {
				httpReq.Header.Set("anthropic-beta", betas)
			}
		} else {
			httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
	}
	httpReq.Header.Set("User-Agent", "ccdp/0.1")
	resp, err = c.httpc.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, 0, ctx.Err()
		}
		return nil, true, 0, &RetryableError{Err: fmt.Errorf("llm: request failed: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := providerHTTPError(resp.Status, raw)
		// 429 / 5xx are transient; other 4xx are not.
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		// Honor the provider's Retry-After (seconds or HTTP-date).
		var after time.Duration
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, perr := strconv.Atoi(strings.TrimSpace(ra)); perr == nil && secs > 0 {
				after = time.Duration(secs) * time.Second
			} else if t, perr := http.ParseTime(ra); perr == nil {
				if d := time.Until(t); d > 0 {
					after = d
				}
			}
		}
		statusErr := errors.New(msg)
		if retryable {
			statusErr = &RetryableError{Err: statusErr, RetryAfter: after}
		}
		return nil, retryable, after, statusErr
	}
	// A 200 without an SSE content-type is not a stream: gateways return
	// 200+JSON error bodies, and providers that ignore the stream parameter
	// reply with a regular completion. Surfacing it beats silently returning
	// an empty result.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		defer resp.Body.Close()
		return nil, false, 0, nonStreamError(resp, ct)
	}
	return resp, false, 0, nil
}

// reasoningPhase tracks whether the model is currently in its reasoning phase,
// so every wire closes it at the same boundaries: the first answer text, the
// first tool/function call fragment, the terminal finish event, and the end of
// the stream. end() reports the close to the caller as an empty text delta: the
// runtime finalizes the open thinking cell on it without starting an answer
// cell. Without the signal the thinking cell only settles when an unrelated
// event arrives, which leaves the UI showing "Thinking…" while the model streams
// tool arguments or the tail of the reply.
type reasoningPhase struct {
	onDelta func(string)
	open    bool
}

// start marks a reasoning phase as open; it is idempotent.
func (p *reasoningPhase) start() { p.open = true }

// end closes an open reasoning phase and tells the caller. Repeated calls
// without a new start are no-ops, so every boundary can call it unconditionally.
func (p *reasoningPhase) end() {
	if !p.open {
		return
	}
	p.open = false
	if p.onDelta != nil {
		p.onDelta("")
	}
}

// markClosed closes the phase without a signal of its own: the caller is about
// to deliver a non-empty answer delta, which already finalizes the thinking cell
// downstream.
func (p *reasoningPhase) markClosed() { p.open = false }

// streamOnce performs a single request attempt. The second return reports
// whether the failure is transient and worth retrying; the third reports
// whether any delta was already delivered to the caller (which rules out a
// retry, since a fresh attempt would replay the text from the beginning); the
// fourth returns a provider-advertised Retry-After delay when present.
func (c *Client) streamOnce(ctx context.Context, body []byte, onDelta func(string), onReasoning func(string)) (result StreamResult, retryable bool, emitted bool, retryAfter time.Duration, err error) {
	if c.isResponses() {
		return c.responsesStreamOnce(ctx, body, onDelta, onReasoning)
	}
	if c.isAnthropic() {
		return c.anthropicStreamOnce(ctx, body, onDelta, onReasoning)
	}
	var resp *http.Response
	resp, retryable, retryAfter, err = c.postStream(ctx, c.baseURL+"/chat/completions", body)
	if err != nil {
		return StreamResult{}, retryable, false, retryAfter, err
	}
	defer resp.Body.Close()

	var (
		toolCalls = map[int]*ToolCall{} // index → call being assembled
		callOrder []int                 // preserve first-seen order of indexes
		thinking  = &reasoningPhase{onDelta: onDelta}
	)
	emitDelta := func(s string) {
		if s == "" {
			return
		}
		result.Text += s
		emitted = true
		thinking.markClosed()
		if onDelta != nil {
			onDelta(s)
		}
	}
	emitReasoning := func(s string) {
		if s == "" {
			return
		}
		result.Reasoning += s
		emitted = true
		thinking.start()
		if onReasoning != nil {
			onReasoning(s)
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	completed := false
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return result, false, emitted, 0, ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			completed = true
			break
		}
		var chunk StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Some providers emit non-JSON keep-alive lines; ignore them.
			continue
		}
		if chunk.Error != nil {
			return StreamResult{}, false, emitted, 0, fmt.Errorf("llm: provider error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			result.PromptTokens = chunk.Usage.PromptTokens
			result.CompletionTok = chunk.Usage.CompletionTokens
			result.CacheReported = chunk.Usage.PromptDetails != nil && chunk.Usage.PromptDetails.CachedTokens != nil
			result.CachedTokens = 0
			if result.CacheReported {
				result.CachedTokens = *chunk.Usage.PromptDetails.CachedTokens
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			result.FinishReason = choice.FinishReason
			completed = true
			thinking.end()
		}
		delta := choice.Delta
		if delta.Content != nil && *delta.Content != "" {
			emitDelta(*delta.Content)
		}
		if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
			emitReasoning(*delta.ReasoningContent)
		}
		if len(delta.ToolCalls) > 0 {
			// The model has moved on to the call itself: its arguments can
			// stream for a long time, so thinking must not stay open.
			thinking.end()
		}
		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			call, ok := toolCalls[idx]
			if !ok {
				call = &ToolCall{Type: "function", Function: Function{}}
				toolCalls[idx] = call
				callOrder = append(callOrder, idx)
			}
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if tc.Function.Name != "" {
				call.Function.Name += tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				call.Function.Arguments += tc.Function.Arguments
				emitted = true // tool-call bytes reached the wire too
			}
		}
	}

	// The stream can end while reasoning is still open (broken connection, or a
	// provider that sent no further boundary event): the caller must not be left
	// holding an unfinished thinking cell.
	thinking.end()

	// Materialize tool calls in first-seen order, skipping incomplete ones.
	for _, idx := range callOrder {
		call := toolCalls[idx]
		if call.ID == "" || call.Function.Name == "" {
			continue
		}
		result.ToolCalls = append(result.ToolCalls, *call)
	}

	if err := scanner.Err(); err != nil {
		return result, true, emitted, 0, &RetryableError{Err: fmt.Errorf("llm: read stream: %w", err)}
	}
	if !completed {
		return result, true, emitted, 0, &RetryableError{Err: fmt.Errorf("llm: stream ended before completion marker")}
	}
	return result, false, emitted, 0, nil
}

// providerHTTPError extracts the useful message from OpenAI-compatible JSON
// error envelopes. Dumping the full indented response into a narrow terminal
// wastes most of the viewport and obscures the actionable part.
func providerHTTPError(status string, raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Message != "" {
		return fmt.Sprintf("llm: %s: %s", status, envelope.Error.Message)
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		body = "empty response"
	}
	return fmt.Sprintf("llm: %s: %s", status, body)
}

// nonStreamError builds a descriptive error for a 200 response that is not an
// SSE stream: a JSON body carrying an "error" object is reported as a
// structured provider error, a "choices" body means the provider ignored the
// stream parameter, and anything else is reported with a body summary.
func nonStreamError(resp *http.Response, contentType string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	body := strings.TrimSpace(string(raw))

	var probe struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Choices []any `json:"choices"`
	}
	isJSON := len(body) > 0 && json.Unmarshal([]byte(body), &probe) == nil
	if isJSON && probe.Error != nil {
		msg := probe.Error.Message
		if msg == "" {
			msg = body
		}
		return fmt.Errorf("llm: provider error (%d): %s", resp.StatusCode, msg)
	}
	if isJSON && len(probe.Choices) > 0 {
		return errors.New("llm: provider returned non-streaming response; check base_url/model")
	}
	if body == "" {
		return fmt.Errorf("llm: provider returned an empty %s response (content-type %q); check base_url/model", resp.Status, contentType)
	}
	summary := body
	if len(summary) > 256 {
		summary = summary[:256] + "…"
	}
	return fmt.Errorf("llm: unexpected non-streaming response (content-type %q): %s", contentType, summary)
}

// EstimateTokens is a token-count heuristic used for compaction decisions when
// the provider reports no usage. Latin text runs ≈4 chars/token; CJK and other
// multibyte scripts count closer to 1 char/token, so each rune contributes its
// byte length in quarter-token units and the total is divided by four
// (1 byte → 1/4 token, 3-4 bytes → 1 token).
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	quarters := 0
	for _, r := range s {
		switch {
		case r < 0x80:
			quarters += 1 // 1 byte ≈ ¼ token
		case r < 0x800:
			quarters += 2
		default:
			quarters += 4 // 3-4 byte runes ≈ 1 token each
		}
	}
	if quarters == 0 {
		return 0
	}
	t := quarters / 4
	if quarters%4 != 0 {
		t++
	}
	if t < 1 {
		t = 1
	}
	return t
}
