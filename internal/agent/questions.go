package agent

import (
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
	if t.ag.children.childState != nil && t.ag.children.childState.nonInteractive {
		return "", errors.New("AskUserQuestion is unavailable in a non-interactive child; report the clarification needed to the parent")
	}
	request.ID = nextRuntimeID("question")
	result := t.ag.awaitDecision(&decisionRequest{kind: decisionQuestion, ctx: ctx.Context, question: protocol.CloneQuestionRequest(&request)})
	if result.err != nil {
		return "", result.err
	}
	data, err := json.Marshal(struct {
		Questions []protocol.Question       `json:"questions"`
		Answers   []protocol.QuestionAnswer `json:"answers,omitempty"`
		Cancelled bool                      `json:"cancelled,omitempty"`
	}{request.Questions, result.answer.Answers, result.answer.Cancelled})
	return string(data), err
}

func (a *Agent) applyAnswerQuestion(cmd protocol.Command) protocol.Receipt {
	a.mu.Lock()
	request, response := a.decisions.pendingQuestion, a.decisions.questionResp
	if request == nil || response == nil || request.ID != cmd.Answer.RequestID {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorNotFound, "question is no longer pending")
	}
	if err := request.ValidateAnswer(*cmd.Answer); err != nil {
		a.mu.Unlock()
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, err.Error())
	}
	response <- protocol.CloneAnswer(*cmd.Answer)
	a.decisions.questionResp, a.decisions.pendingQuestion = nil, nil
	a.mu.Unlock()
	a.finishPendingDecision(decisionQuestion, decisionResult{answer: protocol.CloneAnswer(*cmd.Answer)})
	a.publishState()
	return a.receipt(cmd, protocol.ReceiptApplied, "", nil)
}
