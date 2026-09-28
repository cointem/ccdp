package commands

import (
	"ccdp/internal/protocol"
	"fmt"
	"strconv"
	"strings"
)

// ParseCodingWorkflow is shared by terminal commands and the CLI dispatcher.
func ParseCodingWorkflow(name string, args []string) (protocol.WorkflowCommand, error) {
	w := protocol.WorkflowCommand{}
	switch name {
	case "review":
		w.Kind = protocol.WorkflowReview
		scopes := 0
		for i, arg := range args {
			if arg == "--staged" || arg == "--base" || arg == "--commit" || arg == "--scope" || (i == 0 && (arg == "staged" || arg == "uncommitted" || arg == "branch" || arg == "commit")) {
				scopes++
			}
		}
		if scopes > 1 {
			return w, fmt.Errorf("review scope options are mutually exclusive")
		}
		w.Scope = "uncommitted"
		if len(args) > 0 && (args[0] == "list" || args[0] == "report" || args[0] == "save") {
			w.Action = args[0]
			args = args[1:]
			if (w.Action == "report" || w.Action == "save") && len(args) > 0 {
				w.ID = args[0]
				args = args[1:]
			}
			break
		}
		if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
			w.Scope = args[0]
			args = args[1:]
			if (w.Scope == "branch" || w.Scope == "commit") && len(args) > 0 && !strings.HasPrefix(args[0], "--") {
				if w.Scope == "branch" {
					w.Base = args[0]
				} else {
					w.Commit = args[0]
				}
				args = args[1:]
			}
		}
	case "rewind", "merge":
		w.Kind = protocol.WorkflowRestore
		if name == "merge" {
			w.Kind = protocol.WorkflowMerge
		}
		w.Action = "list"
		if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
			w.Action = args[0]
			args = args[1:]
		}
		if w.Action != "list" && len(args) > 0 && !strings.HasPrefix(args[0], "--") {
			w.ID = args[0]
			args = args[1:]
		}
	case "index":
		w.Kind = protocol.WorkflowIndex
		if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
			w.Action = args[0]
			args = args[1:]
		}
	case "deliver", "commit-push-pr":
		w.Kind = protocol.WorkflowCommitPushPR
		w.Action = "prepare"
		if len(args) > 0 {
			switch args[0] {
			case "prepare", "inspect", "apply", "resume":
				w.Action = args[0]
				args = args[1:]
			}
		}
		if w.Action == "apply" || w.Action == "resume" {
			if len(args) > 0 {
				w.ID = args[0]
				args = args[1:]
			}
		} else {
			var message []string
			for len(args) > 0 && !strings.HasPrefix(args[0], "--") {
				message = append(message, args[0])
				args = args[1:]
			}
			w.Message = strings.Join(message, " ")
		}
	default:
		return w, fmt.Errorf("unknown coding command %q", name)
	}
	for len(args) > 0 {
		key := args[0]
		args = args[1:]
		switch key {
		case "--all":
			w.Scope = "all"
			continue
		case "--staged":
			w.Scope = "staged"
			continue
		case "--draft":
			w.Draft = true
			continue
		case "--local":
			if w.Kind != protocol.WorkflowCommitPushPR {
				return w, fmt.Errorf("--local is only supported by delivery commands")
			}
			w.Mode = "commit"
			continue
		}
		if len(args) == 0 {
			return w, fmt.Errorf("%s needs a value", key)
		}
		v := args[0]
		args = args[1:]
		switch key {
		case "--resolve":
			path, choice, ok := strings.Cut(v, "=")
			if !ok {
				return w, fmt.Errorf("--resolve requires path=current or path=target")
			}
			if w.Resolutions == nil {
				w.Resolutions = map[string]string{}
			}
			w.Resolutions[path] = choice
		case "--scope":
			w.Scope = v
		case "--base":
			w.Base = v
			if w.Kind == protocol.WorkflowReview {
				w.Scope = "branch"
			}
		case "--commit":
			w.Commit = v
			if w.Kind == protocol.WorkflowReview {
				w.Scope = "commit"
			}
		case "--parent":
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 {
				return w, fmt.Errorf("invalid parent")
			}
			w.Parent = n
		case "--path":
			w.Paths = append(w.Paths, v)
		case "--hunk":
			w.Hunks = append(w.Hunks, v)
		case "--mode":
			w.Mode = v
		case "--remote":
			w.Remote = v
		case "--message":
			w.Message = v
		case "--title":
			w.Title = v
		case "--body":
			w.Body = v
		case "--plan":
			w.ID = v
		default:
			return w, fmt.Errorf("unknown option %s", key)
		}
	}
	normalized, e := (protocol.Command{ID: "parse", SessionID: "parse", Type: protocol.CommandRunWorkflow, Workflow: &w}).Normalize()
	if e != nil {
		return w, e
	}
	return *normalized.Workflow, nil
}
