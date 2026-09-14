package protocol

import "testing"

func sampleQuestions() []Question {
	return []Question{{Question: "Which storage?", Header: "Storage", Options: []QuestionOption{{Label: "SQLite", Description: "local"}, {Label: "Postgres", Description: "server"}}}}
}

func TestQuestionValidationAndAnswers(t *testing.T) {
	q := &QuestionRequest{ID: "q", Questions: sampleQuestions()}
	if err := ValidateQuestions(q.Questions); err != nil {
		t.Fatal(err)
	}
	for name, answer := range map[string]AnswerQuestion{
		"missing":            {RequestID: "q"},
		"wrong id":           {RequestID: "stale", Cancelled: true},
		"empty":              {RequestID: "q", Answers: []QuestionAnswer{{}}},
		"unknown":            {RequestID: "q", Answers: []QuestionAnswer{{Selected: []string{"MySQL"}}}},
		"duplicate":          {RequestID: "q", Answers: []QuestionAnswer{{Selected: []string{"SQLite", "SQLite"}}}},
		"single":             {RequestID: "q", Answers: []QuestionAnswer{{Selected: []string{"SQLite", "Postgres"}}}},
		"cancel with answer": {RequestID: "q", Cancelled: true, Answers: []QuestionAnswer{{Text: "yes"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if q.ValidateAnswer(answer) == nil {
				t.Fatal("invalid answer accepted")
			}
		})
	}
	for _, answer := range []AnswerQuestion{
		{RequestID: "q", Cancelled: true},
		{RequestID: "q", Answers: []QuestionAnswer{{Text: "需要离线运行"}}},
		{RequestID: "q", Answers: []QuestionAnswer{{Selected: []string{"SQLite"}, Text: "local only"}}},
	} {
		if err := q.ValidateAnswer(answer); err != nil {
			t.Fatal(err)
		}
	}
	q.Questions[0].MultiSelect = true
	if err := q.ValidateAnswer(AnswerQuestion{RequestID: "q", Answers: []QuestionAnswer{{Selected: []string{"SQLite", "Postgres"}}}}); err != nil {
		t.Fatal(err)
	}
	copy := CloneQuestionRequest(q)
	copy.Questions[0].Options[0].Label = "changed"
	if q.Questions[0].Options[0].Label != "SQLite" {
		t.Fatal("shallow request clone")
	}
}

func TestGenerationCommandNormalization(t *testing.T) {
	effort, verbosity := "high", "low"
	cmd := Command{ID: "cmd", SessionID: "s", Type: CommandSetGeneration, Generation: &SetGeneration{ReasoningEffort: &effort, Verbosity: &verbosity}}
	normalized, err := cmd.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	effort = "garbage"
	if *normalized.Generation.ReasoningEffort != "high" {
		t.Fatal("normalization retained caller pointer")
	}
	if _, err := cmd.Normalize(); err == nil {
		t.Fatal("invalid effort accepted")
	}
	cmd.Generation = &SetGeneration{}
	if _, err := cmd.Normalize(); err == nil {
		t.Fatal("empty update accepted")
	}
	cmd.Generation.ReasoningEffort = &verbosity
	cmd.Answer = &AnswerQuestion{RequestID: "q", Cancelled: true}
	if _, err := cmd.Normalize(); err == nil {
		t.Fatal("mixed payload accepted")
	}
}
