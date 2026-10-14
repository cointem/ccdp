package main

import (
	"fmt"
	"strings"

	"ccdp/internal/protocol"
)

func headlessGoalCommand(objective string, budget int64, budgetSet, resume bool, session, prompt, inputFormat string, continueSession bool) (*protocol.GoalCommand, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" && !resume {
		if budgetSet {
			return nil, fmt.Errorf("--goal-budget requires --goal or --resume-goal")
		}
		return nil, nil
	}
	if objective != "" && resume {
		return nil, fmt.Errorf("--goal and --resume-goal are mutually exclusive")
	}
	if resume && session == "" {
		return nil, fmt.Errorf("--resume-goal requires -r <session>")
	}
	if prompt != "" || inputFormat != "text" || continueSession {
		return nil, fmt.Errorf("goal mode cannot be combined with -p, --input-format stream-json or -c")
	}
	if budgetSet && budget <= 0 {
		return nil, fmt.Errorf("--goal-budget must be a positive token count")
	}
	cmd := &protocol.GoalCommand{Action: "set", Objective: objective}
	if resume {
		cmd.Action = "resume"
	}
	if budgetSet {
		cmd.TokenBudget = &budget
	}
	return cmd, nil
}
