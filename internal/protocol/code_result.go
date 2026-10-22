package protocol

import "encoding/json"

// CodeResult is the host-owned result of one code cell. It remains structured
// until model projection so output limits cannot discard execution facts.
type CodeResult struct {
	Outputs   []json.RawMessage `json:"outputs"`
	Calls     int               `json:"calls"`
	Failed    int               `json:"failed"`
	Denied    int               `json:"denied"`
	Cancelled int               `json:"cancelled"`
	Unknown   int               `json:"unknown"`
	Processes []int             `json:"retained_process_ids,omitempty"`
	Note      string            `json:"note,omitempty"`
	Error     string            `json:"error,omitempty"`
}
