package agent

import (
	"errors"

	"ccdp/internal/session"
	"ccdp/internal/supervision"
)

type childRecordRequest struct {
	record supervision.RunRecord
	result chan error
}

// Enqueue is nonblocking with respect to the owner and its journal. Sources
// serialize their captures under the run lock; the owner drains this FIFO even
// after execution cancellation, before its shutdown barriers are released.
func (a *Agent) enqueueChildRecord(record supervision.RunRecord) <-chan error {
	result := make(chan error, 1)
	frozen, err := record.Freeze()
	if err != nil {
		result <- err
		return result
	}
	if a.owner == nil {
		result <- a.commitChildRecord(frozen)
		return result
	}
	a.ensureRun()
	a.mu.Lock()
	select {
	case <-a.owner.done:
		a.mu.Unlock()
		result <- session.ErrClosed
		return result
	default:
	}
	a.owner.childRecords = append(a.owner.childRecords, childRecordRequest{record: frozen, result: result})
	select {
	case a.owner.control <- completionsReady{}:
	default:
	}
	a.mu.Unlock()
	return result
}

func (a *Agent) drainChildRecords() {
	a.mu.Lock()
	pending := a.owner.childRecords
	a.owner.childRecords = nil
	a.mu.Unlock()
	for _, request := range pending {
		err := a.commitChildRecord(request.record)
		request.result <- err
	}
}

func (a *Agent) waitChildRecord(reply <-chan error) error {
	if a.owner == nil {
		return <-reply
	}
	select {
	case err := <-reply:
		return err
	case <-a.owner.done:
		select {
		case err := <-reply:
			return err
		default:
			return session.ErrClosed
		}
	}
}

func (a *Agent) commitChildRecord(record supervision.RunRecord) error {
	p := a.persistenceHandle()
	if p == nil {
		return errors.New("child: parent persistence unavailable")
	}
	var accounted map[string]bool
	if a.owner != nil {
		accounted = a.owner.childAccounted
	}
	if accounted == nil {
		records, err := p.Read(session.Beginning)
		if err != nil {
			return err
		}
		accounted = make(map[string]bool)
		for _, row := range records {
			if fact, ok := row.Event.(*session.ChildRunRecorded); ok && fact.UsageAccounted {
				accounted[string(fact.Child.Run.ID)] = true
			}
		}
		if a.owner != nil {
			a.owner.childAccounted = accounted
		}
	}
	key := string(record.Fact.Child.Run.ID)
	if record.Terminal && accounted[key] {
		return nil
	}
	if record.Terminal || accounted[key] {
		record.Fact.UsageAccounted = true
	}
	_, err := p.CommitEvents(record.TransactionID, record.Fact)
	if err == nil && record.Fact.UsageAccounted {
		accounted[key] = true
	}
	return err
}
