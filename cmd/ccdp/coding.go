package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"ccdp/internal/agent"
	"ccdp/internal/commands"
	"ccdp/internal/config"
	"ccdp/internal/protocol"
)

func isCodingCommand(s string) bool {
	switch s {
	case "review", "deliver", "rewind", "merge", "index":
		return true
	}
	return false
}

// Coding commands use the same durable operations as the terminal. Noninteractive
// execution never implicitly approves tools; grants come from user configuration.
func runCodingCLI(args []string, out, diagnostics io.Writer) int {
	fail := func(e error) int { fmt.Fprintln(diagnostics, e); return 1 }
	name := args[0]
	args = args[1:]
	var workflowArgs []string
	workspace, sessionID, configPath, mode := "", "", "", ""
	for i := 0; i < len(args); i++ {
		key := args[i]
		switch key {
		case "-d", "--workspace", "-r", "--session", "--config-file", "--permission-mode":
			if i+1 == len(args) {
				return fail(fmt.Errorf("%s requires a value", key))
			}
			i++
			switch key {
			case "-d", "--workspace":
				workspace = args[i]
			case "-r", "--session":
				sessionID = args[i]
			case "--config-file":
				configPath = args[i]
			case "--permission-mode":
				mode = args[i]
			}
		default:
			workflowArgs = append(workflowArgs, key)
		}
	}
	w, e := commands.ParseCodingWorkflow(name, workflowArgs)
	if e != nil {
		return fail(e)
	}
	cfg, e := loadConfig(configPath)
	if e != nil {
		return fail(e)
	}
	if workspace != "" {
		cfg.Workspace = workspace
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "."
	}
	cfg.Workspace, e = filepath.Abs(cfg.Workspace)
	if e != nil {
		return fail(e)
	}
	if mode != "" {
		if e = cfg.ApplyCLIOverride("permission_mode", mode); e != nil {
			return fail(e)
		}
	}
	if project, e := config.LoadProjectSettings(cfg.Workspace); e != nil {
		return fail(e)
	} else {
		config.ApplyProjectSettings(&cfg, &project)
	}
	if e = cfg.Validate(); e != nil {
		return fail(e)
	}
	var a *agent.Agent
	if sessionID != "" {
		snap, err := agent.LoadSession(cfg.SessionDir, sessionID)
		if err != nil {
			return fail(err)
		}
		a, e = agent.Resume(&cfg, snap, nil)
	} else {
		a, e = agent.New(&cfg, nil)
	}
	if e != nil {
		return fail(e)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	watch := &headlessWatch{client: a}
	if e = watch.open(ctx); e != nil {
		return fail(e)
	}
	defer watch.close()
	id := nextHeadlessCommandID(name)
	receipt, e := a.Submit(ctx, protocol.Command{ID: id, SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandRunWorkflow, Workflow: &w})
	if e != nil {
		return fail(e)
	}
	if receipt.Rejected() {
		return fail(fmt.Errorf("%s", receipt.Error))
	}
	fmt.Fprintf(diagnostics, "session: %s\n", a.SessionID())
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-tick.C:
			result, err := a.ReadOperation(ctx, id)
			if w.Kind == protocol.WorkflowReview && w.Action == "" {
				result, err = a.ReadReviewOperation(ctx, id)
			}
			if err != nil {
				return fail(err)
			}
			if result.Complete {
				if result.Output != "" {
					fmt.Fprintln(out, result.Output)
				} else {
					_ = json.NewEncoder(out).Encode(result)
				}
				if result.Status == string(protocol.OutcomePartial) {
					return 2
				}
				if result.Status != "success" {
					return fail(fmt.Errorf("%s", result.Error))
				}
				return 0
			}
		case update, ok := <-watch.sub.Updates():
			if !ok {
				return fail(fmt.Errorf("operation subscription closed"))
			}
			if update.Type == protocol.UpdateResyncRequired {
				if e = watch.resync(ctx, ctx, update); e != nil {
					return fail(e)
				}
				continue
			}
			if update.Event != nil && update.Event.Approval != nil {
				_, e = a.Submit(ctx, protocol.Command{ID: nextHeadlessCommandID("deny"), SessionID: protocol.SessionID(a.SessionID()), Type: protocol.CommandApproveTool, Approval: &protocol.ApproveTool{ApprovalID: update.Event.Approval.ID, Approve: false}})
				if e != nil {
					return fail(e)
				}
			}
		}
	}
}
