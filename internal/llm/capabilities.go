package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Capabilities describes the parts of the provider-neutral request contract a
// provider can accept.  The interface that exposes this value is optional:
// older and test providers that only implement Provider keep the historical
// permissive behaviour.  A provider which does describe itself is checked at
// request preparation, before a PreparedCall can be returned.
//
// ContextWindow and MaxOutputTokens are zero when the provider does not
// publish a corresponding finite limit.  ToolCalling, Images, SystemRole and
// Reasoning are meaningful only for providers that opt into this description;
// a zero value therefore means that capability is not advertised.
type Capabilities struct {
	ToolCalling bool `json:"tool_calling"`
	Images      bool `json:"images"`
	// ImageTokenReserve is a bounded provider-specific reservation per image
	// block. It intentionally does not tokenize a base64 data URL as text.
	// Zero uses the conservative provider-neutral default.
	ImageTokenReserve int `json:"image_token_reserve,omitempty"`
	ContextWindow     int `json:"context_window,omitempty"`
	MaxOutputTokens   int `json:"max_output_tokens,omitempty"`
	// DefaultOutputTokens is the provider's preferred output reservation when
	// the request did not carry an explicit max_tokens value.  Zero selects
	// the provider-neutral conservative default, bounded by ContextWindow and
	// MaxOutputTokens when those limits are published.
	DefaultOutputTokens int  `json:"default_output_tokens,omitempty"`
	Reasoning           bool `json:"reasoning,omitempty"`
	SystemRole          bool `json:"system_role"`
	Usage               bool `json:"usage,omitempty"`
}

// CapabilitiesProvider is an optional provider extension.  Providers without
// this method remain valid Provider implementations for compatibility.
type CapabilitiesProvider interface {
	Capabilities() Capabilities
}

// RequestBudget is the deterministic admission accounting for one wire
// request.  It deliberately reports the sources separately: a context-window
// failure should tell an operator whether system instructions, tool schemas,
// history, or image data consumed the budget.
type RequestBudget struct {
	SystemTokens          int
	SchemaTokens          int
	HistoryTokens         int
	ImageTokens           int
	MessageMetadataTokens int
	EnvelopeTokens        int
	InputTokens           int
	ReservedOutputTokens  int
	TotalTokens           int
	ContextWindow         int
}

var (
	// ErrCapabilityUnsupported identifies a request that asks for a feature a
	// described provider explicitly does not implement.
	ErrCapabilityUnsupported = errors.New("llm: unsupported provider capability")
	// ErrContextWindowExceeded identifies an input plus output reservation that
	// cannot fit in the provider's published context window.
	ErrContextWindowExceeded = errors.New("llm: provider context window exceeded")
	// ErrMaxOutputExceeded identifies a MaxTokens reservation above the
	// provider's published generation limit.
	ErrMaxOutputExceeded = errors.New("llm: provider maximum output exceeded")
)

// AdmissionError retains the capability or budget that rejected a request.
// Callers can use errors.Is with the sentinel errors above while still showing
// the actionable source counts to a user or journal.
type AdmissionError struct {
	Provider   string
	Capability string
	Budget     RequestBudget
	Limit      int
	Detail     string
	Cause      error
}

func (e *AdmissionError) Error() string {
	if e == nil {
		return "llm: request admission failed"
	}
	if e.Detail != "" {
		return "llm: request admission failed: " + e.Detail
	}
	if e.Capability != "" {
		return fmt.Sprintf("llm: request admission failed: provider %q does not support %s", e.Provider, e.Capability)
	}
	return "llm: request admission failed"
}

func (e *AdmissionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ProviderCapabilitiesOf returns a copy of a provider's optional capability
// description.  The bool distinguishes an absent description from a
// deliberately restrictive zero-value description.
func ProviderCapabilitiesOf(provider Provider) (Capabilities, bool) {
	if provider == nil {
		return Capabilities{}, false
	}
	if described, ok := provider.(CapabilitiesProvider); ok {
		return described.Capabilities(), true
	}
	return Capabilities{}, false
}

// AdmitRequest checks a normalized CompletionRequest against provider
// capabilities and returns the accounting used for the decision.  It is safe
// to call more than once; it performs no provider or network operation.
func AdmitRequest(provider Provider, req CompletionRequest) (RequestBudget, error) {
	caps, described := ProviderCapabilitiesOf(provider)
	req = requestWithProviderDefaults(req, caps, described)
	budget := estimateRequestBudget(req, caps.ContextWindow, caps.ImageTokenReserve)
	providerName := ""
	if provider != nil {
		providerName = provider.Name()
	}
	if req.MaxTokens != nil && *req.MaxTokens < 0 {
		return budget, &AdmissionError{Provider: providerName, Detail: "max output reservation must be non-negative", Budget: budget, Cause: ErrMaxOutputExceeded}
	}
	if !described {
		return budget, nil
	}
	hasImages := requestHasImages(req)
	hasTools := len(req.Tools) > 0 || requestHistoryHasToolCalls(req)
	hasSystem := requestHasSystemRole(req)
	if hasTools && !caps.ToolCalling {
		return budget, &AdmissionError{Provider: providerName, Capability: "tool calling", Budget: budget, Cause: ErrCapabilityUnsupported}
	}
	if hasImages && !caps.Images {
		return budget, &AdmissionError{Provider: providerName, Capability: "image input", Budget: budget, Cause: ErrCapabilityUnsupported}
	}
	if hasSystem && !caps.SystemRole {
		return budget, &AdmissionError{Provider: providerName, Capability: "system messages", Budget: budget, Cause: ErrCapabilityUnsupported}
	}
	if caps.MaxOutputTokens > 0 && budget.ReservedOutputTokens > caps.MaxOutputTokens {
		return budget, &AdmissionError{Provider: providerName, Detail: fmt.Sprintf("reserved output %d exceeds maximum %d", budget.ReservedOutputTokens, caps.MaxOutputTokens), Budget: budget, Limit: caps.MaxOutputTokens, Cause: ErrMaxOutputExceeded}
	}
	if caps.ContextWindow > 0 && budget.TotalTokens > caps.ContextWindow {
		return budget, &AdmissionError{Provider: providerName, Detail: fmt.Sprintf("request uses %d tokens including %d reserved output, context window is %d", budget.TotalTokens, budget.ReservedOutputTokens, caps.ContextWindow), Budget: budget, Limit: caps.ContextWindow, Cause: ErrContextWindowExceeded}
	}
	return budget, nil
}

// ValidateRequest is the error-only form of AdmitRequest for callers that do
// not need the accounting details.
func ValidateRequest(provider Provider, req CompletionRequest) error {
	_, err := AdmitRequest(provider, req)
	return err
}

// EstimateRequestBudget counts every model-visible wire contribution. The
// estimate is intentionally conservative and provider-neutral: text uses the
// existing character/rune heuristic, structured values are encoded once so
// tool schemas and custom content cannot disappear from accounting, and every
// image block receives a bounded modality reservation rather than treating a
// base64 data URL as ordinary text. This is an admission estimate, not a
// provider tokenizer result; actual usage remains authoritative when reported.
func EstimateRequestBudget(req CompletionRequest, contextWindow int) RequestBudget {
	return estimateRequestBudget(req, contextWindow, 0)
}

const (
	// Image bytes are not model-visible text.  A large base64 data URL must not
	// consume millions of text tokens, but an image still needs a conservative
	// finite reservation for visual encoding.  Providers may publish a more
	// suitable estimate through Capabilities.ImageTokenReserve.
	defaultImageTokenReserve  = 4096
	defaultOutputTokenReserve = 4096
)

// requestWithProviderDefaults materializes the output budget that a provider
// will receive when the caller leaves MaxTokens unset.  Keeping this on the
// request (rather than only in the estimate) makes the selected reservation
// visible in the frozen wire manifest and gives the provider an actual
// max_tokens ceiling.  A provider without a finite context window remains
// compatible with the legacy provider-default behavior.
func requestWithProviderDefaults(req CompletionRequest, caps Capabilities, described bool) CompletionRequest {
	if !described || req.MaxTokens != nil || caps.ContextWindow <= 0 {
		return req
	}
	reserve := caps.DefaultOutputTokens
	if reserve <= 0 {
		reserve = defaultOutputTokenReserve
	}
	if caps.MaxOutputTokens > 0 && reserve > caps.MaxOutputTokens {
		reserve = caps.MaxOutputTokens
	}
	// Leave room for input on small windows.  The provider may still reject a
	// request whose input alone exceeds its window; this only avoids reserving
	// an output budget larger than the window itself.
	if windowReserve := max(caps.ContextWindow/4, 1); reserve > windowReserve {
		reserve = windowReserve
	}
	if reserve > 0 {
		req.MaxTokens = &reserve
	}
	return req
}

func estimateRequestBudget(req CompletionRequest, contextWindow, imageTokenReserve int) RequestBudget {
	if imageTokenReserve <= 0 {
		imageTokenReserve = defaultImageTokenReserve
	}
	b := RequestBudget{ContextWindow: contextWindow}
	// These constants account for the provider-neutral chat envelope.  Exact
	// tokenization is provider-specific, so the admission contract uses a
	// deterministic conservative floor for delimiters in addition to every
	// model-visible field below.
	const (
		requestEnvelopeTokens = 2
		messageEnvelopeTokens = 3
		toolEnvelopeTokens    = 2
	)
	b.EnvelopeTokens = requestEnvelopeTokens + len(req.Messages)*messageEnvelopeTokens + len(req.Tools)*toolEnvelopeTokens
	for _, message := range req.Messages {
		var textTokens, imageTokens int
		textTokens, imageTokens = estimateContentWithReserve(message.Content, imageTokenReserve)
		if strings.EqualFold(message.Role, "system") {
			b.SystemTokens += textTokens
		} else {
			b.HistoryTokens += textTokens
		}
		b.ImageTokens += imageTokens
		b.HistoryTokens += EstimateTokens(message.ReasoningContent)
		b.MessageMetadataTokens += EstimateTokens(message.Role)
		b.MessageMetadataTokens += EstimateTokens(message.Name)
		b.MessageMetadataTokens += EstimateTokens(message.ToolCallID)
		if len(message.ToolCalls) > 0 {
			encoded, _ := json.Marshal(message.ToolCalls)
			b.HistoryTokens += EstimateTokens(string(encoded))
		}
	}
	for _, tool := range req.Tools {
		encoded, _ := json.Marshal(tool)
		b.SchemaTokens += EstimateTokens(string(encoded))
	}
	b.InputTokens = b.SystemTokens + b.SchemaTokens + b.HistoryTokens + b.ImageTokens + b.MessageMetadataTokens + b.EnvelopeTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		b.ReservedOutputTokens = *req.MaxTokens
	}
	b.TotalTokens = b.InputTokens + b.ReservedOutputTokens
	return b
}

func estimateContent(content any) (textTokens, imageTokens int) {
	return estimateContentWithReserve(content, defaultImageTokenReserve)
}

func estimateContentWithReserve(content any, imageTokenReserve int) (textTokens, imageTokens int) {
	if imageTokenReserve <= 0 {
		imageTokenReserve = defaultImageTokenReserve
	}
	switch value := content.(type) {
	case nil:
		return 0, 0
	case string:
		return EstimateTokens(value), 0
	case ContentPart:
		return estimateContentPart(value, imageTokenReserve)
	case *ContentPart:
		if value == nil {
			return 0, 0
		}
		return estimateContentPart(*value, imageTokenReserve)
	case []ContentPart:
		for _, part := range value {
			t, i := estimateContentPart(part, imageTokenReserve)
			textTokens += t
			imageTokens += i
		}
		return textTokens, imageTokens
	case []any:
		for _, part := range value {
			t, i := estimateContentWithReserve(part, imageTokenReserve)
			textTokens += t
			imageTokens += i
		}
		return textTokens, imageTokens
	default:
		// Custom provider-neutral content is still model-visible. Walk common
		// JSON containers to separate image blocks; fall back to its encoded
		// representation when it is not one of those containers.
		if valueMap, ok := value.(map[string]any); ok {
			if typ, _ := valueMap["type"].(string); strings.EqualFold(typ, "image") || strings.EqualFold(typ, "image_url") {
				return 0, imageTokenReserve
			}
			for key, child := range valueMap {
				t, i := estimateContentWithReserve(child, imageTokenReserve)
				if strings.EqualFold(key, "image_url") || strings.EqualFold(key, "image") {
					i = max(i, imageTokenReserve)
					t = 0
				}
				textTokens += t
				imageTokens += i
			}
			return textTokens, imageTokens
		}
		if valueSlice, ok := value.([]map[string]any); ok {
			for _, child := range valueSlice {
				t, i := estimateContentWithReserve(child, imageTokenReserve)
				textTokens += t
				imageTokens += i
			}
			return textTokens, imageTokens
		}
		encoded, _ := json.Marshal(value)
		return EstimateTokens(string(encoded)), 0
	}
}

func estimateContentPart(part ContentPart, imageTokenReserve int) (int, int) {
	if strings.EqualFold(part.Type, "image") || strings.EqualFold(part.Type, "image_url") || part.ImageURL != nil {
		return 0, max(imageTokenReserve, 1)
	}
	return EstimateTokens(part.Text), 0
}

func requestHasImages(req CompletionRequest) bool {
	for _, message := range req.Messages {
		if contentHasImage(message.Content) {
			return true
		}
	}
	return false
}

// contentHasImage is deliberately independent from the byte/token estimate:
// an empty image URL is still an image capability request and must not become
// an invisible zero-token value.
func contentHasImage(content any) bool {
	switch value := content.(type) {
	case ContentPart:
		return strings.EqualFold(value.Type, "image") || strings.EqualFold(value.Type, "image_url") || value.ImageURL != nil
	case *ContentPart:
		return value != nil && (strings.EqualFold(value.Type, "image") || strings.EqualFold(value.Type, "image_url") || value.ImageURL != nil)
	case []ContentPart:
		for _, part := range value {
			if contentHasImage(part) {
				return true
			}
		}
	case []any:
		for _, part := range value {
			if contentHasImage(part) {
				return true
			}
		}
	case []map[string]any:
		for _, part := range value {
			if contentHasImage(part) {
				return true
			}
		}
	case map[string]any:
		if typ, _ := value["type"].(string); strings.EqualFold(typ, "image") || strings.EqualFold(typ, "image_url") {
			return true
		}
		for key, child := range value {
			if strings.EqualFold(key, "image") || strings.EqualFold(key, "image_url") || contentHasImage(child) {
				return true
			}
		}
	}
	return false
}

func requestHasSystemRole(req CompletionRequest) bool {
	for _, message := range req.Messages {
		if strings.EqualFold(message.Role, "system") {
			return true
		}
	}
	return false
}

func requestHistoryHasToolCalls(req CompletionRequest) bool {
	for _, message := range req.Messages {
		if len(message.ToolCalls) > 0 || strings.EqualFold(message.Role, "tool") {
			return true
		}
	}
	return false
}
