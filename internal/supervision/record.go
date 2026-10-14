package supervision

import (
	"encoding/json"

	"ccdp/internal/session"
)

// RunRecord is a frozen request to the parent session's single writer. The
// directory enqueues while holding its run lock, then releases that lock before
// waiting for the receipt. Thus concurrent changes preserve their capture order
// without a directory lock being held across an owner call.
type RunRecord struct {
	TransactionID string
	Fact          session.ChildRunRecorded
	Terminal      bool
}

func (r RunRecord) Freeze() (RunRecord, error) {
	data, err := json.Marshal(r.Fact)
	if err != nil {
		return RunRecord{}, err
	}
	var fact session.ChildRunRecorded
	if err := json.Unmarshal(data, &fact); err != nil {
		return RunRecord{}, err
	}
	r.Fact = fact
	return r, nil
}
