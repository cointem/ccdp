package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

const DefaultMaxOutputTokens = 32000

// DefaultReasoningEffort is the built-in reasoning depth applied when no
// global reasoning_effort override is configured. Providers that do not
// support a given level reject the request explicitly; silently running at an
// unknown depth is worse.
const DefaultReasoningEffort = "medium"

// ModelConfig is the single configuration record for one provider's API model.
type ModelConfig struct {
	ContextWindow   int      `json:"context_window,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Pricing         *Pricing `json:"pricing,omitempty"`
}

func (p *ProviderConfig) UnmarshalJSON(data []byte) error {
	type plain ProviderConfig
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range []string{"context_window", "context_windows"} {
		if _, ok := raw[key]; ok {
			return fmt.Errorf("provider.%s is obsolete; put context_window inside each models.<model> object", key)
		}
	}
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("provider: models must be an object keyed by model name: %w", err)
	}
	if value.ModelConfigs == nil || len(value.ModelConfigs) == 0 {
		return fmt.Errorf("provider.models must contain at least one model object")
	}
	*p = ProviderConfig(value)
	for model := range p.ModelConfigs {
		p.Models = append(p.Models, model)
	}
	if models, ok := raw["models"]; ok {
		var perModel map[string]map[string]json.RawMessage
		if json.Unmarshal(models, &perModel) == nil {
			for model, fields := range perModel {
				if _, ok := fields["reasoning_effort"]; ok {
					return fmt.Errorf("provider.models.%s: per-model reasoning_effort is obsolete; reasoning depth is the single global reasoning_effort setting (default %q)", model, DefaultReasoningEffort)
				}
			}
		}
	}
	sort.Strings(p.Models)
	return nil
}

func rejectLegacyModelFields(raw map[string]json.RawMessage) error {
	for _, key := range []string{"api_key", "base_url", "wire_api", "context_window", "context_windows", "pricing", "max_reply_tokens"} {
		if _, ok := raw[key]; ok {
			return fmt.Errorf("config: top-level %s is obsolete; configure providers.<id> and providers.<id>.models.<model>; select model as provider/model", key)
		}
	}
	return nil
}

// APIModelFor removes only a known provider prefix. API IDs may themselves
// contain slashes (for example organization/model).
func (c *Config) APIModelFor(model string) string {
	if id, _, ok := c.servingProviderForModel(model); ok {
		return strings.TrimPrefix(model, id+"/")
	}
	return model
}

func (c *Config) ModelConfigFor(model string) ModelConfig {
	if _, p, ok := c.servingProviderForModel(model); ok {
		return p.ModelConfigs[c.APIModelFor(model)]
	}
	return ModelConfig{}
}

func (c *Config) MaxOutputTokensFor(model string) int {
	if n := c.ModelConfigFor(model).MaxOutputTokens; n > 0 {
		return n
	}
	return DefaultMaxOutputTokens
}

func (c *Config) ReasoningEffortFor(model string) string {
	if c.ReasoningEffort != "" {
		return c.ReasoningEffort
	}
	return DefaultReasoningEffort
}

func (c *Config) PricingFor(model string) Pricing {
	if p := c.ModelConfigFor(model).Pricing; p != nil {
		return *p
	}
	if p, ok := c.Pricing[model]; ok {
		return p
	}
	if p, ok := c.Pricing[c.APIModelFor(model)]; ok {
		return p
	}
	return c.Pricing[c.Model]
}

// ValidateModelSelection is also applied to interactive model switches, where
// a misspelled provider must never silently select the global endpoint.
func (c *Config) ValidateModelSelection(model string) error {
	newFormat := false
	for _, p := range c.Providers {
		if p.ModelConfigs != nil {
			newFormat = true
			break
		}
	}
	if !newFormat {
		return nil
	} // in-process/plugin configurations have their own routes
	id, name, ok := strings.Cut(model, "/")
	if !ok || id == "" || name == "" {
		return fmt.Errorf("model %q must use provider/model", model)
	}
	p, ok := c.Providers[id]
	if !ok {
		return fmt.Errorf("unknown model provider %q", id)
	}
	if _, ok := p.ModelConfigs[name]; !ok {
		return fmt.Errorf("unknown model %q in provider %q", name, id)
	}
	return nil
}

func (c *Config) validateModelConfigs() error {
	for id, p := range c.Providers {
		if p.ModelConfigs == nil {
			continue
		}
		if id == "" || strings.Contains(id, "/") {
			return fmt.Errorf("invalid provider id %q", id)
		}
		if strings.TrimSpace(p.BaseURL) == "" {
			return fmt.Errorf("providers.%s.base_url is required", id)
		}
		if !validWireAPI(p.WireAPI) {
			return fmt.Errorf("providers.%s.wire_api must be chat, responses, or anthropic", id)
		}
		if p.APIKey != "" && p.APIKeyEnv != "" {
			return fmt.Errorf("providers.%s: use api_key or api_key_env, not both", id)
		}
		for name, m := range p.ModelConfigs {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("providers.%s: model name is empty", id)
			}
			if m.ContextWindow != 0 && m.ContextWindow < 4000 {
				return fmt.Errorf("providers.%s.models.%s.context_window must be at least 4000", id, name)
			}
			effectiveOutput := m.MaxOutputTokens
			if effectiveOutput == 0 {
				effectiveOutput = DefaultMaxOutputTokens
			}
			if m.MaxOutputTokens < 0 || effectiveOutput >= c.ContextWindowFor(id+"/"+name) {
				return fmt.Errorf("providers.%s.models.%s: max_output_tokens must be below context_window (default is %d); set a smaller per-model limit", id, name, DefaultMaxOutputTokens)
			}
			if m.Pricing != nil && (m.Pricing.Input < 0 || m.Pricing.Output < 0) {
				return fmt.Errorf("providers.%s.models.%s: negative pricing", id, name)
			}
		}
	}
	if err := c.ValidateModelSelection(c.Model); err != nil {
		return err
	}
	if c.FallbackModel != "" {
		return c.ValidateModelSelection(c.FallbackModel)
	}
	return nil
}

func providerEnvKey(p ProviderConfig) string {
	if p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return p.APIKey
}
