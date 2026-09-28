package session

import "errors"

type DeliveryRecorded struct {
	PlanID    string `json:"plan_id"`
	Stage     string `json:"stage"`
	Commit    string `json:"commit,omitempty"`
	Index     string `json:"index,omitempty"`
	PR        string `json:"pr,omitempty"`
	Error     string `json:"error,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	Tree      string `json:"tree,omitempty"`
	Checks    string `json:"checks,omitempty"`
}

const EventTypeDeliveryRecorded EventType = "DeliveryRecorded"

func (DeliveryRecorded) Type() EventType { return EventTypeDeliveryRecorded }
func (DeliveryRecorded) seal()           {}
func (e DeliveryRecorded) validate() error {
	if len(e.PlanID) != 64 {
		return errors.New("invalid delivery plan")
	}
	switch e.Stage {
	case "prepared", "checking", "checked", "committing", "candidate_committed", "publishing", "committed", "push_requested", "pushed", "pr_requested", "completed", "recovery_required":
		return nil
	}
	return errors.New("invalid delivery stage")
}
