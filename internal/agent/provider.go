package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/plugin"
)

type requestJournalMetadata struct {
	Config           config.Config
	SettingsRevision uint64
	ContextRevision  uint64
	CatalogVersion   uint64
	Turn             uint64
	Step             uint64
	HistoryLen       int
}

type requestJournalMetadataKey struct{}

const (
	// Runtime retries are deliberately owned by this journal boundary. The
	// HTTP adapter is configured OneAttempt, so every retry below receives its
	// own RequestPrepared/AttemptFinished facts and usage cannot disappear
	// inside one opaque provider call.
	runtimeProviderMaxRetries = 3
	runtimeProviderRetryDelay = 500 * time.Millisecond
)

func withRequestJournalMetadata(ctx context.Context, metadata requestJournalMetadata) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestJournalMetadataKey{}, metadata)
}

func requestJournalMetadataFrom(ctx context.Context) (requestJournalMetadata, bool) {
	if ctx == nil {
		return requestJournalMetadata{}, false
	}
	metadata, ok := ctx.Value(requestJournalMetadataKey{}).(requestJournalMetadata)
	return metadata, ok
}

// httpProviderFor creates the built-in OpenAI-compatible adapter for exactly
// one model from the caller's already-resolved binding. The endpoint, key and
// wire format arrive as one value so a model's credential can never be paired
// with a different provider's wire format.
func httpProviderFor(cfg config.Config, model string, binding plugin.HTTPBinding) (llm.Provider, error) {
	client, err := llm.NewClient(llm.Config{
		BaseURL:         binding.Endpoint,
		APIKey:          binding.APIKey,
		Model:           model,
		APIModel:        cfg.APIModelFor(model),
		ContextWindow:   cfg.ContextWindow,
		MaxOutputTokens: cfg.MaxOutputTokensFor(model),
		Timeout:         10 * time.Minute,
		// The runtime journals one RequestPrepared/AttemptFinished pair around
		// each provider call. Keep HTTP retry decisions at that boundary so a
		// transient failure cannot disappear inside one opaque Stream call.
		OneAttempt: true,
		Wire:       binding.Wire,
		Debug:      cfg.Debug,
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// resolvedRoute is the atomic provider identity for one model: the adapter, its
// public endpoint, where the route came from, and — for a generated HTTP
// adapter — the wire format and the config provider record that produced the
// whole tuple. A plugin route carries neither wire nor providerID because
// neither is derived from the HTTP config.
type resolvedRoute struct {
	client     llm.Provider
	endpoint   string
	routeKind  string
	wire       string
	providerID string
}

// resolveProviderForModel resolves a provider and its complete endpoint/key/wire
// binding as one unit. Explicit plugin routes always win. If no route exists, an
// HTTP adapter is created for this model's own config binding and registered
// under that model. In particular, a sole unrelated registry provider is never
// used for a model with a different configured endpoint.
func resolveProviderForModel(cfg config.Config, models *plugin.ModelRegistry, model string) (resolvedRoute, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return resolvedRoute{}, errors.New("agent: model is required")
	}
	if err := cfg.ValidateModelSelection(model); err != nil {
		return resolvedRoute{}, err
	}
	cfg.ContextWindow = cfg.ContextWindowFor(model)
	resolved := cfg.ResolveProvider(model)
	binding := plugin.HTTPBinding{Endpoint: resolved.BaseURL, APIKey: resolved.APIKey, Wire: resolved.Wire}
	if models != nil {
		if p, ok := models.ResolveDefault(model, binding); ok && p != nil && httpCapsMatch(cfg, model, p) {
			return resolvedRoute{client: providerWithOperatorCaps(cfg, model, p), endpoint: binding.Endpoint, routeKind: "http", wire: binding.Wire, providerID: resolved.ID}, nil
		}
		if p, routeKind, routeEndpoint, ok := models.ResolveRouteInfo(model); ok && p != nil {
			// A generated HTTP route is reusable only when ResolveDefault above
			// matched the complete endpoint/key/wire binding. If config was reloaded,
			// let the branch below create a fresh adapter for this model instead
			// of inheriting the stale route. Explicit plugin routes are always
			// authoritative and never borrow cfg's HTTP endpoint.
			if routeKind == "http" {
				// Continue to the model-specific HTTP factory below.
			} else {
				pluginEndpoint := ""
				if endpointProvider, ok := p.(llm.EndpointProvider); ok {
					pluginEndpoint = endpointProvider.Endpoint()
				} else {
					pluginEndpoint = routeEndpoint
				}
				// Plugin providers do not receive the operator's context/output
				// limits through their own constructor. Attach those limits to the
				// immutable binding while retaining the provider's optional feature
				// description; an undescribed provider keeps the historical
				// permissive tool/image/system behaviour but is still bounded by
				// the operator window and reply cap.
				return resolvedRoute{client: providerWithOperatorCaps(cfg, model, p), endpoint: pluginEndpoint, routeKind: "plugin"}, nil
			}
		}
	}
	p, err := httpProviderFor(cfg, model, binding)
	if err != nil {
		return resolvedRoute{}, err
	}
	if models != nil {
		// Registering a per-model adapter is what makes later preparations use
		// the same binding without invoking Resolve's sole-provider fallback.
		models.Register(p)
		models.RouteDefault(model, p.Name(), binding)
	}
	return resolvedRoute{client: p, endpoint: binding.Endpoint, routeKind: "http", wire: binding.Wire, providerID: resolved.ID}, nil
}

// Recreate generated adapters when configured budgets change, including increases.
func httpCapsMatch(cfg config.Config, model string, p llm.Provider) bool {
	caps, ok := llm.ProviderCapabilitiesOf(p)
	return ok && caps.ContextWindow == cfg.ContextWindow && caps.MaxOutputTokens == cfg.MaxOutputTokensFor(model)
}

// operatorCapsProvider is the small adapter used for explicit plugin routes.
// It exists only at the frozen model-binding edge: the registry continues to
// hold the plugin's original provider, while each binding carries the
// operator's immutable context/output ceilings into llm.NewPreparedCall.
// Embedding is avoided so the optional reasoning surface can be delegated
// without exposing a second request path.
type operatorCapsProvider struct {
	provider llm.Provider
	caps     llm.Capabilities
}

func (p *operatorCapsProvider) Name() string { return p.provider.Name() }

func (p *operatorCapsProvider) Capabilities() llm.Capabilities { return p.caps }

func (p *operatorCapsProvider) Stream(ctx context.Context, req llm.CompletionRequest, onDelta func(string)) (llm.StreamResult, error) {
	return p.provider.Stream(ctx, req, onDelta)
}

func (p *operatorCapsProvider) StreamWithReasoning(ctx context.Context, req llm.CompletionRequest, onDelta func(string), onReasoning func(string)) (llm.StreamResult, error) {
	if reasoning, ok := p.provider.(llm.ReasoningProvider); ok {
		return reasoning.StreamWithReasoning(ctx, req, onDelta, onReasoning)
	}
	return p.provider.Stream(ctx, req, onDelta)
}

func providerWithOperatorCaps(cfg config.Config, model string, provider llm.Provider) llm.Provider {
	if provider == nil || (cfg.ContextWindow <= 0 && cfg.MaxOutputTokensFor(model) <= 0) {
		return provider
	}
	caps, described := llm.ProviderCapabilitiesOf(provider)
	if !described {
		// An old provider has no feature declaration. Keep its pre-admission
		// compatibility while adding only the operator's explicit ceilings.
		caps = llm.Capabilities{ToolCalling: true, Images: true, Reasoning: true, SystemRole: true, Usage: true}
	}
	if cfg.ContextWindow > 0 && (caps.ContextWindow <= 0 || cfg.ContextWindow < caps.ContextWindow) {
		caps.ContextWindow = cfg.ContextWindow
	}
	if cfg.MaxOutputTokensFor(model) > 0 && (caps.MaxOutputTokens <= 0 || cfg.MaxOutputTokensFor(model) < caps.MaxOutputTokens) {
		caps.MaxOutputTokens = cfg.MaxOutputTokensFor(model)
	}
	// Avoid wrapping an HTTP client (or another provider already carrying the
	// same ceilings) so registry identity and its optional methods remain
	// unchanged where no additional operator restriction is needed.
	if described {
		providerCaps, _ := llm.ProviderCapabilitiesOf(provider)
		if providerCaps.ContextWindow == caps.ContextWindow && providerCaps.MaxOutputTokens == caps.MaxOutputTokens {
			return provider
		}
	}
	return &operatorCapsProvider{provider: provider, caps: caps}
}

// providerBinding builds an immutable binding without consulting mutable Agent
// state after the config/registry snapshots have been captured.
func providerBinding(cfg config.Config, models *plugin.ModelRegistry, model string, version uint64) (modelBinding, error) {
	route, err := resolveProviderForModel(cfg, models, model)
	if err != nil {
		return modelBinding{}, fmt.Errorf("resolve provider for %s: %w", model, err)
	}
	return modelBinding{
		model:      model,
		provider:   route.client.Name(),
		endpoint:   route.endpoint,
		routeKind:  route.routeKind,
		wire:       route.wire,
		providerID: route.providerID,
		client:     route.client,
		version:    version,
	}, nil
}

// prepareProviderCall is the only request-to-provider boundary. The request
// has already been built from a step snapshot; NewPreparedCall then freezes
// both the complete wire input and the provider pointer before any network or
// plugin code runs.
func prepareProviderCall(binding modelBinding, req llm.CompletionRequest) (llm.PreparedCall, error) {
	if binding.client == nil {
		return nil, errors.New("agent: provider binding is unavailable")
	}
	req.Stream = true
	return llm.NewPreparedCall(binding.client, binding.provider, binding.endpoint, req)
}

// requestJournalFailure marks admission/finish persistence failures so the
// main loop can stop instead of treating them as provider failures eligible
// for fallback. A journal failure must never be hidden by a second provider
// call.
type requestJournalFailure struct{ err error }

func (e *requestJournalFailure) Error() string {
	if e == nil || e.err == nil {
		return "request journal failed"
	}
	return "request journal failed: " + e.err.Error()
}

func (e *requestJournalFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func isRequestJournalFailure(err error) bool {
	var target *requestJournalFailure
	return errors.As(err, &target)
}

// streamPrepared records a prepared request before invoking its provider and
// closes each attempt only after the provider returns. Usage and provider
// failures are passed to the journal once per actual provider attempt;
// transient failures are retried here, outside the HTTP adapter, so every
// retry remains auditable. Callers must not invoke recordUsage for a result
// returned by this function.
func (a *Agent) streamPrepared(ctx context.Context, purpose string, call llm.PreparedCall, metadata requestJournalMetadata, onDelta func(string), onReasoning func(string)) (llm.StreamResult, error) {
	if call == nil {
		return llm.StreamResult{}, &requestJournalFailure{err: errors.New("prepared call is nil")}
	}
	defer call.Close()
	manifest := call.Manifest()
	req, err := llm.PreparedRequest(call)
	if err != nil {
		return llm.StreamResult{}, &requestJournalFailure{err: err}
	}
	ctx = withRequestJournalMetadata(ctx, metadata)

	for retry := 0; ; retry++ {
		attempt, err := a.recordPreparedRequest(ctx, purpose, manifest.Provider, manifest.Endpoint, req)
		if err != nil {
			return llm.StreamResult{}, &requestJournalFailure{err: err}
		}

		var result llm.StreamResult
		var callErr error
		var callbackEmitted atomic.Bool
		attemptDelta := func(delta string) {
			if delta != "" {
				callbackEmitted.Store(true)
			}
			if onDelta != nil {
				onDelta(delta)
			}
		}
		attemptReasoning := func(delta string) {
			if delta != "" {
				callbackEmitted.Store(true)
			}
			if onReasoning != nil {
				onReasoning(delta)
			}
		}
		if reasoningCall, ok := call.(llm.ReasoningPreparedCall); ok && onReasoning != nil {
			result, callErr = reasoningCall.StreamWithReasoning(ctx, attemptDelta, attemptReasoning)
		} else {
			result, callErr = call.Stream(ctx, attemptDelta)
		}
		result = llm.CloneStreamResult(result)
		if finishErr := a.finishRequestAttempt(attempt, result, callErr); finishErr != nil {
			return result, &requestJournalFailure{err: finishErr}
		}
		if callErr == nil {
			return result, nil
		}
		if retry >= runtimeProviderMaxRetries || ctx.Err() != nil || callbackEmitted.Load() || providerCallEmitted(result) {
			return result, callErr
		}
		retryAfter, retryable := llm.RetryAfter(callErr)
		if !retryable {
			return result, callErr
		}
		delay := runtimeProviderRetryDelay
		if retryAfter > 0 {
			delay = retryAfter
		}
		if delay > time.Minute {
			delay = time.Minute
		}
		if retryAfter <= 0 {
			// retry is the number of the failed attempt, so the first retry uses
			// the base delay and later retries back off exponentially.
			delay *= time.Duration(1 << min(retry, 6))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return result, ctx.Err()
		case <-timer.C:
		}
	}
}

func providerCallEmitted(result llm.StreamResult) bool {
	return result.Text != "" || result.Reasoning != "" || len(result.ToolCalls) > 0
}
