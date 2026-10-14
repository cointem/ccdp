package session

import (
	"errors"

	"ccdp/internal/protocol"
)

// GoalChanged replaces the session goal; nil explicitly clears it.
type GoalChanged struct {
	Goal         *protocol.Goal `json:"goal"`
	AccountingID string         `json:"accounting_id,omitempty"`
}

func (e GoalChanged) Type() EventType { return EventTypeGoalChanged }

func (e GoalChanged) seal() {}

func (e GoalChanged) validate() error {
	if e.Goal == nil {
		if e.AccountingID != "" {
			return errors.New("goal accounting requires a goal")
		}
		return nil
	}
	if e.AccountingID != "" {
		if err := requireID("goal accounting id", e.AccountingID); err != nil {
			return err
		}
	}
	return e.Goal.Validate()
}
