package journal

import (
	"errors"
	"fmt"
	"reflect"

	"ccdp/internal/messages"
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

func (p *Writer) MemoryText() (string, bool, error) {
	if p == nil {
		return "", false, errors.New("agent: nil session persistence")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	projection, err := p.projectionLocked()
	if err != nil {
		return "", false, err
	}
	return projection.MemoryText, projection.MemorySeen, nil
}

func (p *Writer) PersistSettingsWorkflow(settings session.Settings, revision uint64, workflow *session.WorkflowState) error {
	p.mu.Lock()
	if p.store == nil {
		p.mu.Unlock()
		return session.ErrClosed
	}
	if p.failed != nil {
		err := p.failed
		p.mu.Unlock()
		return err
	}
	projection, err := p.projectionLocked()
	if err != nil {
		p.mu.Unlock()
		return fmt.Errorf("agent: inspect settings projection: %w", err)
	}
	events := make([]session.Event, 0, 2)
	if !reflect.DeepEqual(projection.Settings, settings) || revision > projection.SettingsRev {
		if revision == 0 {
			revision = projection.SettingsRev + 1
			if revision == 0 {
				revision = 1
			}
		}
		events = append(events, session.SettingsChanged{Revision: revision, Settings: settings})
	}
	if workflow != nil && *workflow != projection.Workflow {
		events = append(events, session.WorkflowChanged{Workflow: *workflow})
	}
	if len(events) == 0 {
		p.mu.Unlock()
		return nil
	}
	if _, err = p.commitLocked(session.Batch{TransactionID: messages.StableID("settings-workflow", struct {
		Settings session.Settings
		Revision uint64
		Workflow *session.WorkflowState
		Cursor   session.Cursor
	}{settings, revision, workflow, p.store.CurrentCursor()}), Events: events}); err != nil {
		p.mu.Unlock()
		return fmt.Errorf("agent: persist settings/workflow: %w", err)
	}
	p.mu.Unlock()
	return nil
}

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
