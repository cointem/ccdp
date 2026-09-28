package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
	"context"
	"errors"
)

type OperationResult struct {
	ID       protocol.CommandID `json:"id"`
	Complete bool               `json:"complete"`
	Status   string             `json:"status"`
	Output   string             `json:"output"`
	Error    string             `json:"error,omitempty"`
}

func (a *Agent) ReadOperation(ctx context.Context, id protocol.CommandID) (OperationResult, error) {
	out := OperationResult{ID: id}
	if e := ctx.Err(); e != nil {
		return out, e
	}
	p := a.persistenceHandle()
	if p == nil {
		return out, errors.New("session unavailable")
	}
	rows, e := p.Read(session.Beginning)
	if e != nil {
		return out, e
	}
	for _, row := range rows {
		var done *session.CommandCompleted
		switch v := row.Event.(type) {
		case *session.CommandCompleted:
			done = v
		case session.CommandCompleted:
			done = &v
		}
		if done == nil || done.CommandID != string(id) {
			continue
		}
		out.Complete = true
		out.Status = done.Outcome
		out.Error = done.Report
		if done.Output != nil {
			blobs, e := p.requestArtifacts()
			if e != nil {
				return out, e
			}
			b, e := blobs.Read(*done.Output, session.DefaultMaxBlobBytes)
			if e != nil {
				return out, e
			}
			out.Output = string(b)
		}
		return out, nil
	}
	return out, nil
}
