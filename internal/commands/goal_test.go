package commands

import "testing"

func TestParseGoal(t *testing.T) {
	tests := []struct {
		text, action, objective string
		budget                  int64
		hasBudget, bad          bool
	}{
		{text: ""},
		{text: `Fix Bob's "broken" test at C:\src\main.go`, action: "set", objective: `Fix Bob's "broken" test at C:\src\main.go`},
		{text: "-- edit the command", action: "set", objective: "edit the command"},
		{text: "--budget 900 -- pause all tasks", action: "set", objective: "pause all tasks", budget: 900, hasBudget: true},
		{text: "edit new\nobjective", action: "edit", objective: "new\nobjective"},
		{text: "pause", action: "pause"}, {text: "resume", action: "resume"}, {text: "clear", action: "clear"},
		{text: "budget off", action: "budget", hasBudget: true},
		{text: "budget 120", action: "budget", budget: 120, hasBudget: true},
		{text: "pause now", bad: true}, {text: "--budget 0 task", bad: true}, {text: "--budget -1 task", bad: true},
		{text: "--budget 9223372036854775808 task", bad: true}, {text: "--budget 30", bad: true}, {text: "edit", bad: true},
		{text: "budget", bad: true}, {text: "budget off extra", bad: true}, {text: "--unknown task", bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			command, err := ParseGoal(tt.text)
			if (err != nil) != tt.bad {
				t.Fatalf("command=%+v err=%v", command, err)
			}
			if tt.bad {
				return
			}
			if tt.action == "" {
				if command != nil {
					t.Fatal(command)
				}
				return
			}
			if command.Action != tt.action || command.Objective != tt.objective || (command.TokenBudget != nil) != tt.hasBudget {
				t.Fatalf("got %+v", command)
			}
			if command.TokenBudget != nil && *command.TokenBudget != tt.budget {
				t.Fatal(*command.TokenBudget)
			}
		})
	}
}
