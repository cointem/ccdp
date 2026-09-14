package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ReasoningProvider is an optional extension implemented by providers that
// expose a separate reasoning stream. Providers that do not implement it are
// still valid Provider implementations; callers should treat reasoning as an
// optional preview channel.
type ReasoningProvider interface {
	Provider
	StreamWithReasoning(context.Context, CompletionRequest, func(string), func(string)) (StreamResult, error)
}

// EndpointProvider is an optional diagnostic extension. Generic plugins may
// omit it; in that case request manifests leave Endpoint empty rather than
// pretending an unrelated HTTP config URL is the plugin's endpoint.
type EndpointProvider interface {
	Endpoint() string
}

// DeltaSink receives one text fragment from a streaming provider.
type DeltaSink func(string)

// ReasoningSink receives one reasoning fragment from a provider that supports
// the optional ReasoningProvider extension.
type ReasoningSink func(string)

// RequestManifest is the durable, provider-neutral description of a prepared
// call. RequestJSON contains the complete serialized request, including
// stream=true. Authentication is intentionally not included.
//
// The manifest is a value object: callers receive a deep copy from Manifest so
// mutating a returned byte slice cannot alter the prepared call.
type RequestManifest struct {
	Provider    string          `json:"provider"`
	Endpoint    string          `json:"endpoint,omitempty"`
	Model       string          `json:"model"`
	RequestJSON json.RawMessage `json:"request_json"`
}

// PreparedCall is a request whose provider and complete wire input were
// resolved and frozen together. It deliberately does not accept another
// request at Stream time, preventing a mutable config/history read from
// changing the request after admission.
type PreparedCall interface {
	Manifest() RequestManifest
	Stream(context.Context, DeltaSink) (StreamResult, error)
	Close() error
}

// ReasoningPreparedCall is the optional richer form returned by providers that
// can stream reasoning deltas. The ordinary PreparedCall contract remains the
// common boundary for all providers.
type ReasoningPreparedCall interface {
	PreparedCall
	StreamWithReasoning(context.Context, DeltaSink, ReasoningSink) (StreamResult, error)
}

// NewPreparedCall freezes req and binds it to provider. It first serializes
// req for validation, decodes that body with UseNumber, and serializes the
// typed request again as the stable wire source. This keeps custom JSON values
// deterministic while preserving CompletionRequest's field order: the
// provider receives a request reconstructed from exactly these bytes, so its
// own JSON encoding matches Manifest.RequestJSON. Callers may safely reuse or
// mutate their original request and any nested maps/slices after this returns.
func NewPreparedCall(provider Provider, providerName, endpoint string, req CompletionRequest) (PreparedCall, error) {
	if provider == nil {
		return nil, errors.New("llm: provider is required")
	}
	if providerName == "" {
		providerName = provider.Name()
	}
	// Materialize the bounded provider-default output reservation before the
	// first marshal.  This keeps the request admitted and the frozen manifest
	// on the same wire contract; a nil MaxTokens must not let input consume the
	// entire finite context window.
	caps, described := ProviderCapabilitiesOf(provider)
	req = requestWithProviderDefaults(req, caps, described)
	// The serialized bytes are the sole frozen request source. In particular,
	// do not clone through an unconstrained reflection walk first: cycles and
	// unsupported values must be reported by this marshal instead of recursing
	// or leaving a shallow pointer behind.
	frozen := req
	frozen.Stream = true
	raw, err := json.Marshal(frozen)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal prepared request: %w", err)
	}
	normalizedRequest, err := decodePreparedRequest(raw)
	if err != nil {
		return nil, fmt.Errorf("llm: normalize prepared request: %w", err)
	}
	normalized, err := json.Marshal(normalizedRequest)
	if err != nil {
		return nil, fmt.Errorf("llm: normalize prepared request: %w", err)
	}
	// Capability and window admission happens after normalization, while the
	// complete provider-neutral wire request is still local and before a
	// PreparedCall can be returned.  This prevents an adapter from seeing a
	// request with unsupported tools/images or an over-sized input and keeps
	// the budget accounting tied to exactly the bytes that will be sent.
	if _, err := AdmitRequest(provider, normalizedRequest); err != nil {
		return nil, err
	}
	manifest := RequestManifest{
		Provider:    providerName,
		Endpoint:    endpoint,
		Model:       frozen.Model,
		RequestJSON: append(json.RawMessage(nil), normalized...),
	}
	return &preparedCall{
		provider:    provider,
		requestJSON: append([]byte(nil), normalized...),
		manifest:    manifest,
	}, nil
}

type preparedCall struct {
	mu          sync.Mutex
	provider    Provider
	requestJSON []byte
	manifest    RequestManifest
	closed      bool
}

func (c *preparedCall) Manifest() RequestManifest {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.manifest
	m.RequestJSON = append(json.RawMessage(nil), c.manifest.RequestJSON...)
	return m
}

func (c *preparedCall) Stream(ctx context.Context, onDelta DeltaSink) (StreamResult, error) {
	return c.stream(ctx, onDelta, nil)
}

func (c *preparedCall) StreamWithReasoning(ctx context.Context, onDelta DeltaSink, onReasoning ReasoningSink) (StreamResult, error) {
	return c.stream(ctx, onDelta, onReasoning)
}

func (c *preparedCall) stream(ctx context.Context, onDelta DeltaSink, onReasoning ReasoningSink) (StreamResult, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return StreamResult{}, errors.New("llm: prepared call is closed")
	}
	p := c.provider
	raw := append([]byte(nil), c.requestJSON...)
	c.mu.Unlock()
	req, err := decodePreparedRequest(raw)
	if err != nil {
		return StreamResult{}, fmt.Errorf("llm: decode prepared request: %w", err)
	}

	var result StreamResult
	var callErr error
	if rp, ok := p.(ReasoningProvider); ok && onReasoning != nil {
		result, callErr = rp.StreamWithReasoning(ctx, req, onDelta, onReasoning)
	} else {
		result, callErr = p.Stream(ctx, req, onDelta)
	}
	return CloneStreamResult(result), callErr
}

func (c *preparedCall) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

// decodePreparedRequest reconstructs a fresh request from the exact bytes
// captured at preparation. Content is deliberately kept as JSON-shaped
// values. Converting arbitrary arrays to []ContentPart can silently discard
// provider-specific fields or change object key order; the normalized JSON is
// the source of truth for both the manifest and provider request. A malformed
// manifest is treated as a call error rather than reaching a provider with a
// partially decoded request.
func decodePreparedRequest(raw []byte) (CompletionRequest, error) {
	var req CompletionRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return CompletionRequest{}, err
	}
	if !req.Stream {
		return CompletionRequest{}, errors.New("prepared request has stream=false")
	}
	return req, nil
}

// PreparedRequest returns a deep copy decoded from the manifest's frozen
// bytes. It is useful to journal the exact request without exposing concrete
// implementation details of a PreparedCall.
func PreparedRequest(call PreparedCall) (CompletionRequest, error) {
	if call == nil {
		return CompletionRequest{}, errors.New("llm: prepared call is nil")
	}
	manifest := call.Manifest()
	return decodePreparedRequest(manifest.RequestJSON)
}

func cloneToolCalls(src []ToolCall) []ToolCall {
	if src == nil {
		return nil
	}
	dst := make([]ToolCall, len(src))
	copy(dst, src)
	return dst
}

// CloneStreamResult copies the only reference-bearing field in StreamResult.
func CloneStreamResult(src StreamResult) StreamResult {
	dst := src
	dst.ToolCalls = cloneToolCalls(src.ToolCalls)
	return dst
}
