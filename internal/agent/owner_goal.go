package agent

import (
	"ccdp/internal/protocol"
	"ccdp/internal/session"
)

type goalTransition uint8

const (
	goalApplyPending goalTransition = iota
	goalFinishTurn
	goalRecordResponse
	goalAdmitAttempt
	goalCreate
	goalUpdate
	goalPause
	goalCancelWork
)

type goalTransitionRequested struct {
	transition       goalTransition
	id, text, status string
	toolCalls        int
	budget           *int64
	result           chan goalTransitionResult
}

func (goalTransitionRequested) ownerControlMessage() {}

type goalTransitionResult struct {
	goal     *protocol.Goal
	admitted bool
	err      error
}

func (a *Agent) requestGoalTransition(request goalTransitionRequested) goalTransitionResult {
	if a.owner == nil {
		return a.applyGoalTransition(request)
	}
	request.result = make(chan goalTransitionResult, 1)
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return goalTransitionResult{err: session.ErrClosed}
	}
	select {
	case result := <-request.result:
		return result
	case <-a.owner.done:
		return goalTransitionResult{err: session.ErrClosed}
	}
}
func (a *Agent) applyGoalTransition(request goalTransitionRequested) (result goalTransitionResult) {
	switch request.transition {
	case goalApplyPending:
		result.err = a.applyPendingGoalEditOwned()
	case goalFinishTurn:
		a.finishGoalTurnOwned()
	case goalRecordResponse:
		a.recordGoalResponseOwned(request.text, request.toolCalls)
	case goalAdmitAttempt:
		result.admitted = a.admitGoalAttemptOwned(request.id)
	case goalCreate:
		result.goal, result.err = (goalToolControl{agent: a}).createGoalOwned(request.text, request.budget)
	case goalUpdate:
		result.goal, result.err = (goalToolControl{agent: a, goalID: request.id}).updateGoalOwned(request.status, request.text)
	case goalPause:
		a.pauseGoal(request.text)
	case goalCancelWork:
		a.cancelWork(request.text)
	}
	return result
}
func (a *Agent) applyPendingGoalEdit() error {
	return a.requestGoalTransition(goalTransitionRequested{transition: goalApplyPending}).err
}
func (a *Agent) finishGoalTurn() {
	a.requestGoalTransition(goalTransitionRequested{transition: goalFinishTurn})
}
func (a *Agent) recordGoalResponse(text string, toolCalls int) {
	a.requestGoalTransition(goalTransitionRequested{transition: goalRecordResponse, text: text, toolCalls: toolCalls})
}
func (a *Agent) admitGoalAttempt(id string) bool {
	return a.requestGoalTransition(goalTransitionRequested{transition: goalAdmitAttempt, id: id}).admitted
}
func (a *Agent) pauseGoalFromWorker(reason string) {
	a.requestGoalTransition(goalTransitionRequested{transition: goalPause, text: reason})
}
func (a *Agent) cancelWorkFromWorker(reason string) {
	a.requestGoalTransition(goalTransitionRequested{transition: goalCancelWork, text: reason})
}
func (c goalToolControl) CreateGoal(objective string, budget *int64) (*protocol.Goal, error) {
	if budget != nil {
		value := *budget
		budget = &value
	}
	result := c.agent.requestGoalTransition(goalTransitionRequested{transition: goalCreate, text: objective, budget: budget})
	return result.goal, result.err
}
func (c goalToolControl) UpdateGoal(status, reason string) (*protocol.Goal, error) {
	result := c.agent.requestGoalTransition(goalTransitionRequested{transition: goalUpdate, id: c.goalID, status: status, text: reason})
	return result.goal, result.err
}

type goalAttemptCompleted struct {
	manifest session.RequestManifest
	attempt  session.Attempt
	result   chan error
}

func (goalAttemptCompleted) ownerControlMessage() {}
func (a *Agent) settleGoalAttempt(manifest session.RequestManifest, attempt session.Attempt) error {
	request := goalAttemptCompleted{manifest: manifest, attempt: attempt, result: make(chan error, 1)}
	if a.owner == nil {
		return a.accountGoalFinishedOwned(manifest, attempt, true)
	}
	a.ensureRun()
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}

type sharedAttemptCompleted struct {
	attempt session.Attempt
	result  chan error
}

func (sharedAttemptCompleted) ownerControlMessage() {}
func (a *Agent) settleSharedAttempt(attempt session.Attempt) error {
	if a.owner == nil {
		return a.accountSharedAttempt(attempt)
	}
	a.ensureRun()
	request := sharedAttemptCompleted{attempt: attempt, result: make(chan error, 1)}
	select {
	case a.owner.control <- request:
	case <-a.owner.done:
		return session.ErrClosed
	}
	select {
	case err := <-request.result:
		return err
	case <-a.owner.done:
		return session.ErrClosed
	}
}
