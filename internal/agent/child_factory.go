package agent

import (
	"context"
	"errors"

	"ccdp/internal/config"
	"ccdp/internal/messages"
	"ccdp/internal/plugin"
	"ccdp/internal/supervision"
	"ccdp/internal/tools"
)

// childFactory is prepared once for one run. Construction only receives its
// frozen configuration and borrowed capabilities, never the parent Agent.
// The run owns the retained catalog lease until the returned runtime closes.
type childFactory struct {
	config        config.Config
	options       Options
	initial       *SessionSnapshot
	history       []messages.Message
	historySource historyArtifactSource
	tools         *tools.Lease
	preHooks      []plugin.PreToolHook
	recoveryID    string
}

func (f childFactory) Open(ctx context.Context) (supervision.ChildRuntime, error) {
	child, err := f.openAgent(ctx)
	if err != nil {
		return nil, err
	}
	return child, nil
}

func (f childFactory) openAgent(ctx context.Context) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options := f.options
	options.RootContext = ctx
	var child *Agent
	var err error
	if f.initial == nil && len(f.history) > 0 {
		child, err = newForkAgent(&f.config, options, f.historySource, f.history)
	} else {
		child, err = newAgentWithOptions(&f.config, f.initial, options)
	}
	if err != nil {
		return nil, err
	}
	if f.initial != nil {
		if err := child.cancelRunInputs(f.recoveryID); err != nil {
			return nil, errors.Join(err, child.CloseContext(context.Background()))
		}
	}
	child.inheritBorrowedTools(f.tools)
	child.inheritBorrowedPreHooks(f.preHooks)
	return child, nil
}

var _ supervision.ChildFactory = childFactory{}
