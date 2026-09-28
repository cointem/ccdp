package agent

import (
	"context"
	"strings"
)

type reviewTextRule struct {
	exclude   string
	normalize bool
}

// Query only the next admission batch. Filters are never executed, and rejected
// paths cannot consume space that should remain available to readable context.
func (j *reviewJob) textRules(ctx context.Context, paths []string, autocrlf bool) (map[string]reviewTextRule, error) {
	args := append([]string{"check-attr", "-z", "text", "eol", "filter", "working-tree-encoding", "--"}, paths...)
	raw, err := j.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(string(raw), "\x00")
	attrs := map[string]map[string]string{}
	for i := 0; i+2 < len(fields); i += 3 {
		if attrs[fields[i]] == nil {
			attrs[fields[i]] = map[string]string{}
		}
		attrs[fields[i]][fields[i+1]] = fields[i+2]
	}
	rules := map[string]reviewTextRule{}
	specified := func(s string) bool { return s != "" && s != "unspecified" && s != "unset" }
	for _, p := range paths {
		at := attrs[p]
		rule := reviewTextRule{}
		switch {
		case specified(at["filter"]):
			rule.exclude = "custom Git filter requires transformation; not executed"
		case specified(at["working-tree-encoding"]) && !strings.EqualFold(at["working-tree-encoding"], "UTF-8"):
			rule.exclude = "working-tree-encoding " + at["working-tree-encoding"] + " is not supported by review"
		default:
			rule.normalize = at["text"] != "unset" && (autocrlf || specified(at["text"]) || specified(at["eol"]))
		}
		rules[p] = rule
	}
	return rules, nil
}
