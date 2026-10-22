package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBudgetIncludesNativeReasoningWithoutCountingOpaqueBytes(t *testing.T) {
	visible := strings.Repeat("think carefully ", 300)
	raw, _ := json.Marshal(map[string]any{"type": "thinking", "thinking": visible, "signature": strings.Repeat("signature", 100000)})
	req := CompletionRequest{Messages: []ChatMessage{{Role: "assistant", Content: "done", ProviderContent: []json.RawMessage{raw}}}}
	budget := EstimateRequestBudget(req, 20000)
	if budget.HistoryTokens < EstimateTokens(visible) || budget.HistoryTokens > EstimateTokens(visible)+100 {
		t.Fatalf("native/opaque accounting = %d", budget.HistoryTokens)
	}
	req.Messages[0].ReasoningContent = visible
	duplicate := EstimateRequestBudget(req, 20000)
	if duplicate.HistoryTokens != budget.HistoryTokens {
		t.Fatalf("native reasoning double counted: %d -> %d", budget.HistoryTokens, duplicate.HistoryTokens)
	}
	req.Messages[0].ProviderContent = []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("secret", 100000) + `"}`)}
	req.Messages[0].ReasoningContent = ""
	if got := EstimateRequestBudget(req, 20000); got.HistoryTokens > 100 {
		t.Fatalf("ciphertext treated as visible tokens: %+v", got)
	}
}

func TestBudgetIncludesLargerNativeSystemBlocks(t *testing.T) {
	req := CompletionRequest{Messages: []ChatMessage{{Role: "system", Content: "short"}}, SystemBlocks: []SystemBlock{{Text: strings.Repeat("instructions ", 200)}}}
	budget := EstimateRequestBudget(req, 20000)
	if budget.SystemTokens != EstimateTokens(req.SystemBlocks[0].Text) {
		t.Fatalf("native system omitted or counted twice: %+v", budget)
	}
}
