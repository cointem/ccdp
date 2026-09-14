package agent

import (
	"ccdp/internal/config"
	"ccdp/internal/llm"
	"ccdp/internal/protocol"
	"net/url"
	"strings"
)

func (a *Agent) applyGeneration(cmd protocol.Command) protocol.Receipt {
	if err := protocol.ValidateGeneration(valueOrEmpty(cmd.Generation.ReasoningEffort), valueOrEmpty(cmd.Generation.Verbosity)); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	a.settingsCommitMu.Lock()
	defer a.settingsCommitMu.Unlock()
	a.mu.Lock()
	if a.busy || a.settling || a.pendingBinding != nil {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorBusy, "change generation settings after the current turn finishes")
	}
	settings := a.sessionSettingsLocked()
	revision := a.settingsRev + 1
	if cmd.Generation.ReasoningEffort != nil {
		settings.ReasoningEffort = *cmd.Generation.ReasoningEffort
	}
	if cmd.Generation.Verbosity != nil {
		settings.Verbosity = *cmd.Generation.Verbosity
	}
	settings.GenerationOptionsSet = true
	a.mu.Unlock()
	if err := a.persistSettingsFact(settings, revision); err != nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInternal, err.Error())
	}
	a.mu.Lock()
	a.cfg.ReasoningEffort, a.baseCfg.ReasoningEffort = settings.ReasoningEffort, settings.ReasoningEffort
	a.cfg.Verbosity, a.baseCfg.Verbosity = settings.Verbosity, settings.Verbosity
	a.settingsRev = revision
	a.mu.Unlock()
	a.publishState()
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
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
	return strings.HasPrefix(strings.ToLower(model), "deepseek") || (parsed != nil && strings.EqualFold(parsed.Hostname(), "api.deepseek.com"))
}

func applyGenerationRequest(cfg config.Config, req *llm.CompletionRequest) {
	req.ReasoningEffort, req.Verbosity = cfg.ReasoningEffort, cfg.Verbosity
	if isDeepSeekModel(cfg, req.Model) && cfg.ReasoningEffort != "" {
		req.Thinking = &llm.ThinkingConfig{Type: "enabled"}
		if cfg.ReasoningEffort == "none" {
			req.Thinking.Type = "disabled"
			req.ReasoningEffort = ""
		}
	}
}
