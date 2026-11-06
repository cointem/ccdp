package journal

import (
	"errors"
	"fmt"
	"reflect"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func (p *Writer) PersistGoal(goal *protocol.Goal) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	goal = protocol.CloneGoal(goal)
	if goal != nil {
		if err := goal.Validate(); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed != nil {
		return fmt.Errorf("%w: %v", session.ErrPersistenceFailed, p.failed)
	}
	if p.store == nil {
		return session.ErrClosed
	}
	projection, err := p.projectionLocked()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(projection.Goal, goal) {
		return nil
	}
	_, err = p.commitLocked(session.Batch{TransactionID: messages.StableID("goal", struct {
		Goal   *protocol.Goal
		Cursor session.Cursor
	}{goal, p.store.CurrentCursor()}), Events: []session.Event{session.GoalChanged{Goal: goal}}})
	return err
}

func (p *Writer) PersistUsage(usage session.Usage) error {
	if p == nil {
		return errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store == nil {
		return session.ErrClosed
	}
	records, err := session.ReadAll(p.store)
	if err != nil {
		return err
	}
	var last *session.UsageChanged
	for _, record := range records {
		if event, ok := record.Event.(*session.UsageChanged); ok {
			copy := *event
			last = &copy
		}
	}
	converted := usage
	if last != nil && last.Usage == converted {
		return nil
	}
	if usage == (session.Usage{}) {
		return nil
	}
	baseCursor := p.store.CurrentCursor()
	_, err = p.commitLocked(session.Batch{TransactionID: "usage-" + messages.StableID("usage", struct {
		Usage  session.Usage
		Cursor session.Cursor
	}{converted, baseCursor}), Events: []session.Event{session.UsageChanged{Revision: uint64(len(records) + 1), Usage: converted}}})
	return err
}
