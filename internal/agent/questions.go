package agent

import (
	"context"
	"encoding/json"
	"errors"

	"ccdp/internal/protocol"
	"ccdp/internal/tools"
)

type askUserQuestionTool struct{ ag *Agent }

func (*askUserQuestionTool) Name() string { return "AskUserQuestion" }
func (*askUserQuestionTool) Description() string {
	return "Ask the user 1–4 structured clarification questions, each with 2–4 choices. Supports single or multiple selections and free-text answers. Available in plan mode: use it to resolve requirements before submitting a plan. This is not permission to execute tools. Do not include an Other option; the UI always accepts free text. Cancellation supplies no answer; never infer consent."
}
func (*askUserQuestionTool) Parameters() map[string]any {
	var schema map[string]any
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{"question":{"type":"string"},"header":{"type":"string","maxLength":12},"multiSelect":{"type":"boolean"},"options":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"label":{"type":"string"},"description":{"type":"string"}},"required":["label","description"]}}},"required":["question","header","options","multiSelect"]}}},"required":["questions"]}`), &schema)
	return schema
}
func (t *askUserQuestionTool) Run(ctx *tools.Context) (string, error) {
	raw, err := json.Marshal(ctx.Args)
	if err != nil {
		return "", err
	}
	var request protocol.QuestionRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return "", err
	}
	if err := protocol.ValidateQuestions(request.Questions); err != nil {
		return "", err
	}
	if t.ag.childState != nil && t.ag.childState.nonInteractive {
		return "", errors.New("AskUserQuestion is unavailable in a non-interactive child; report the clarification needed to the parent")
	}
	// Interactive tools share a modal slot with permission and plan decisions.
	t.ag.approvalMu.Lock()
	defer t.ag.approvalMu.Unlock()
	waitCtx := ctx.Context
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	if err := waitCtx.Err(); err != nil {
		return "", err
	}
	request.ID = nextRuntimeID("question")
	response := make(chan protocol.AnswerQuestion, 1)
	t.ag.mu.Lock()
	t.ag.pendingQuestion = protocol.CloneQuestionRequest(&request)
	t.ag.questionResp = response
	t.ag.mu.Unlock()
	defer func() {
		t.ag.mu.Lock()
		if t.ag.questionResp == response {
			t.ag.pendingQuestion = nil
			t.ag.questionResp = nil
		}
		t.ag.mu.Unlock()
		t.ag.publishState()
	}()
	t.ag.publishState()
	t.ag.emit(Event{Type: EventQuestion, Question: protocol.CloneQuestionRequest(&request)})
	select {
	case answer := <-response:
		result := struct {
			Questions []protocol.Question       `json:"questions"`
			Answers   []protocol.QuestionAnswer `json:"answers,omitempty"`
			Cancelled bool                      `json:"cancelled,omitempty"`
		}{request.Questions, answer.Answers, answer.Cancelled}
		data, err := json.Marshal(result)
		return string(data), err
	case <-waitCtx.Done():
		return "", waitCtx.Err()
	}
}

func (a *Agent) applyAnswerQuestion(cmd protocol.Command) protocol.Receipt {
	a.mu.Lock()
	request, response := a.pendingQuestion, a.questionResp
	if request == nil || response == nil || request.ID != cmd.Answer.RequestID {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorNotFound, "question is no longer pending")
	}
	if err := request.ValidateAnswer(*cmd.Answer); err != nil {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	response <- protocol.CloneAnswer(*cmd.Answer)
	a.questionResp, a.pendingQuestion = nil, nil
	a.mu.Unlock()
	a.publishState()
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
}
