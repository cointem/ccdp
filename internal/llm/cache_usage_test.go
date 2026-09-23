package llm

import (
	"encoding/json"
	"testing"
)

func TestCacheUsagePresence(t *testing.T) {
	for _, tt := range []struct {
		details string
		known   bool
	}{{``, false}, {`,"prompt_tokens_details":{}`, false}, {`,"prompt_tokens_details":{"cached_tokens":null}`, false}, {`,"prompt_tokens_details":{"cached_tokens":0}`, true}, {`,"prompt_tokens_details":{"cached_tokens":70}`, true}} {
		var u Usage
		if err := json.Unmarshal([]byte(`{"prompt_tokens":100`+tt.details+`}`), &u); err != nil {
			t.Fatal(err)
		}
		known := u.PromptDetails != nil && u.PromptDetails.CachedTokens != nil
		if known != tt.known {
			t.Fatalf("%s: known=%v", tt.details, known)
		}
	}
}
