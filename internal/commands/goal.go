package commands

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"ccdp/internal/protocol"
)

// ParseGoal parses the text after /goal. Objectives are literal prose, not
// shell arguments: quotes, newlines and backslashes retain their meaning.
// A nil command requests the read-only goal report.
func ParseGoal(raw string) (*protocol.GoalCommand, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	word, rest := goalWord(raw)
	cmd := &protocol.GoalCommand{Action: "set"}
	switch word {
	case "pause", "resume", "clear":
		if rest != "" {
			return nil, fmt.Errorf("usage: /goal %s", word)
		}
		cmd.Action = word
		return cmd, nil
	case "edit":
		cmd.Action, cmd.Objective = word, rest
	case "budget":
		budget, err := ParseGoalBudget(rest, true)
		if err != nil {
			return nil, err
		}
		cmd.Action, cmd.TokenBudget = word, &budget
		return cmd, nil
	case "--":
		cmd.Objective = rest
	case "--budget":
		value, objective := goalWord(rest)
		budget, err := ParseGoalBudget(value, false)
		if err != nil {
			return nil, err
		}
		if first, remainder := goalWord(objective); first == "--" {
			objective = remainder
		}
		cmd.TokenBudget, cmd.Objective = &budget, objective
	default:
		if strings.HasPrefix(word, "--") {
			return nil, fmt.Errorf("unknown goal option %q; use /goal -- <literal objective>", word)
		}
		cmd.Objective = raw
	}
	if cmd.Objective == "" {
		return nil, fmt.Errorf("usage: /goal [--budget <tokens>] <objective> | edit <objective> | pause | resume | clear | budget <tokens|off>")
	}
	return cmd, nil
}

func ParseGoalBudget(raw string, allowOff bool) (int64, error) {
	if allowOff && raw == "off" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("goal budget must be a positive token count%s", map[bool]string{true: " or off"}[allowOff])
	}
	return n, nil
}

func goalWord(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if index := strings.IndexFunc(raw, unicode.IsSpace); index >= 0 {
		return raw[:index], strings.TrimSpace(raw[index:])
	}
	return raw, ""
}
