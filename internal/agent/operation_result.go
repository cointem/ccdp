package agent

import (
	"context"
	"errors"

	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type OperationResult = protocol.OperationResult

func (a *Agent) ReadOperation(ctx context.Context, id protocol.CommandID) (OperationResult, error) {
	rows, blobs, err := a.ReadRunRecords(ctx)
	if err != nil {
		return OperationResult{ID: id}, err
	}
	return readOperationResult(ctx, rows, blobs, id)
}

func (a *Agent) ReadInput(ctx context.Context, id protocol.InputID) (protocol.InputResult, error) {
	rows, _, err := a.ReadRunRecords(ctx)
	if err != nil {
		return protocol.InputResult{ID: id}, err
	}
	return readInputResult(ctx, rows, id)
}

func readOperationResult(ctx context.Context, rows []session.Record, blobs outputReader, id protocol.CommandID) (OperationResult, error) {
	out := OperationResult{ID: id}
	if err := ctx.Err(); err != nil {
		return out, err
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
		out.Code = protocol.ErrorCode(done.Code)
		if done.Output != nil {
			if blobs == nil {
				return out, errors.New("operation output unavailable")
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

func readInputResult(ctx context.Context, rows []session.Record, id protocol.InputID) (protocol.InputResult, error) {
	out := protocol.InputResult{ID: id}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		event := row.Event
		switch value := event.(type) {
		case session.InputDelivered:
			event = &value
		case session.InputCancelled:
			event = &value
		case session.TurnFinished:
			event = &value
		}
		switch value := event.(type) {
		case *session.InputDelivered:
			if value.InputID == string(id) {
				out.Delivered, out.TurnID = true, protocol.TurnID(value.TurnID)
			}
		case *session.InputCancelled:
			if value.InputID == string(id) {
				out.Status, out.Error = protocol.TurnCancelled, value.Reason
			}
		case *session.TurnFinished:
			if out.Delivered && value.TurnID == string(out.TurnID) {
				switch value.Outcome {
				case "success":
					out.Status = protocol.TurnSucceeded
				case "cancelled":
					out.Status = protocol.TurnCancelled
				case "limit_reached":
					out.Status = protocol.TurnLimited
				case "blocked":
					out.Status = protocol.TurnBlocked
				default:
					out.Status = protocol.TurnFailed
				}
				out.Error = value.Error
			}
		}
	}
	return out, nil
}
