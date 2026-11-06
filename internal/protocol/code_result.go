package protocol

import "encoding/json"

// CodeResult is the host-owned result of one code cell. It remains structured
// until model projection so output limits cannot discard execution facts.
type CodeResult struct {
	// DenialKey is host-only evidence that every child hit the same access
	// decision. It is never accepted from worker output or exposed on the wire.
	DenialKey string            `json:"-"`
	Completed []ProcessOutput   `json:"process_results,omitempty"`
	Outputs   []json.RawMessage `json:"outputs"`
	Calls     int               `json:"calls"`
	Failed    int               `json:"failed"`
	Denied    int               `json:"denied"`
	Cancelled int               `json:"cancelled"`
	Unknown   int               `json:"unknown"`
	Processes []string          `json:"retained_process_ids,omitempty"`
	Note      string            `json:"note,omitempty"`
	Error     string            `json:"error,omitempty"`
}
