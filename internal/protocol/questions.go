package protocol

import (
	"errors"
	"fmt"
	"strings"
)

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
}

type QuestionRequest struct {
	ID        string     `json:"id"`
	Questions []Question `json:"questions"`
}

type QuestionAnswer struct {
	Selected []string `json:"selected,omitempty"`
	Text     string   `json:"text,omitempty"`
}

type AnswerQuestion struct {
	RequestID string           `json:"request_id"`
	Answers   []QuestionAnswer `json:"answers,omitempty"`
	Cancelled bool             `json:"cancelled,omitempty"`
}

func CloneQuestionRequest(q *QuestionRequest) *QuestionRequest {
	if q == nil {
		return nil
	}
	out := *q
	out.Questions = append([]Question(nil), q.Questions...)
	for i := range out.Questions {
		out.Questions[i].Options = append([]QuestionOption(nil), q.Questions[i].Options...)
	}
	return &out
}

func CloneAnswer(a AnswerQuestion) AnswerQuestion {
	a.Answers = append([]QuestionAnswer(nil), a.Answers...)
	for i := range a.Answers {
		a.Answers[i].Selected = append([]string(nil), a.Answers[i].Selected...)
	}
	return a
}

func ValidateQuestions(questions []Question) error {
	if len(questions) < 1 || len(questions) > 4 {
		return errors.New("AskUserQuestion: provide 1–4 questions")
	}
	seen := map[string]bool{}
	for _, q := range questions {
		if strings.TrimSpace(q.Question) == "" || len(q.Question) > 2000 {
			return errors.New("AskUserQuestion: question is required and must be at most 2000 bytes")
		}
		if seen[q.Question] {
			return errors.New("AskUserQuestion: questions must be unique")
		}
		seen[q.Question] = true
		if strings.TrimSpace(q.Header) == "" || len([]rune(q.Header)) > 12 {
			return errors.New("AskUserQuestion: header must be 1–12 characters")
		}
		if len(q.Options) < 2 || len(q.Options) > 4 {
			return errors.New("AskUserQuestion: provide 2–4 options per question; free text is supplied by the UI")
		}
		labels := map[string]bool{}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" || len(o.Label) > 200 || len(o.Description) > 1000 || labels[o.Label] {
				return errors.New("AskUserQuestion: option labels must be nonempty, unique, and at most 200 bytes; descriptions at most 1000 bytes")
			}
			labels[o.Label] = true
		}
	}
	return nil
}

func (q *QuestionRequest) ValidateAnswer(a AnswerQuestion) error {
	if q == nil || q.ID != a.RequestID {
		return errors.New("question is no longer pending")
	}
	if a.Cancelled {
		if len(a.Answers) != 0 {
			return errors.New("cancelled question cannot include answers")
		}
		return nil
	}
	if len(a.Answers) != len(q.Questions) {
		return errors.New("answer every question")
	}
	for i, answer := range a.Answers {
		question := q.Questions[i]
		if len(answer.Selected) > len(question.Options) || (!question.MultiSelect && len(answer.Selected) > 1) {
			return fmt.Errorf("question %d: too many selections", i+1)
		}
		if len(answer.Text) > 8000 {
			return fmt.Errorf("question %d: text exceeds 8000 bytes", i+1)
		}
		if len(answer.Selected) == 0 && strings.TrimSpace(answer.Text) == "" {
			return fmt.Errorf("question %d: select an option or enter text", i+1)
		}
		seen := map[string]bool{}
		for _, label := range answer.Selected {
			valid := false
			for _, option := range question.Options {
				if label == option.Label {
					valid = true
					break
				}
			}
			if !valid || seen[label] {
				return fmt.Errorf("question %d: invalid or duplicate selection %q", i+1, label)
			}
			seen[label] = true
		}
	}
	return nil
}
