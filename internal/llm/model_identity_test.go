package llm

import (
	"encoding/json"
	"testing"
)

func TestQualifiedIdentityUsesAPIModelOnBothWires(t *testing.T) {
	for _, wire := range []string{"chat", "responses"} {
		client, err := NewClient(Config{Model: "router/org/model", APIModel: "org/model", BaseURL: "https://example.com/v1", Wire: wire})
		if err != nil {
			t.Fatal(err)
		}
		body, err := client.marshalRequestBody(CompletionRequest{Model: "router/org/model"})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got["model"] != "org/model" || client.Name() != "router/org/model" {
			t.Fatalf("%s: identity/wire model mismatch", wire)
		}
	}
}
