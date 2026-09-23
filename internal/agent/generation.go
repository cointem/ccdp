package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"net/url"
	"strings"
)

func (a *Agent) applyGeneration(cmd protocol.Command) protocol.Receipt {
	if err := protocol.ValidateGeneration(valueOrEmpty(cmd.Generation.ReasoningEffort), valueOrEmpty(cmd.Generation.Verbosity)); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	return a.applySettingsCommand(cmd, func(target *session.Settings) error {
		if cmd.Generation.ReasoningEffort != nil {
			target.ReasoningEffort = *cmd.Generation.ReasoningEffort
		}
		if cmd.Generation.Verbosity != nil {
			target.Verbosity = *cmd.Generation.Verbosity
		}
		target.GenerationOptionsSet = true
		return nil
	})
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func isDeepSeekModel(cfg config.Config, model string) bool {
	endpoint, _ := cfg.EndpointFor(model)
	parsed, _ := url.Parse(endpoint)
	return strings.HasPrefix(strings.ToLower(cfg.APIModelFor(model)), "deepseek") || (parsed != nil && strings.EqualFold(parsed.Hostname(), "api.deepseek.com"))
}

func applyGenerationRequest(cfg config.Config, req *llm.CompletionRequest) {
	cfg.ReasoningEffort = cfg.ReasoningEffortFor(req.Model)
	req.ReasoningEffort, req.Verbosity = cfg.ReasoningEffort, cfg.Verbosity
	if isDeepSeekModel(cfg, req.Model) && cfg.ReasoningEffort != "" {
		req.Thinking = &llm.ThinkingConfig{Type: "enabled"}
		if cfg.ReasoningEffort == "none" {
			req.Thinking.Type = "disabled"
			req.ReasoningEffort = ""
		}
	}
}
