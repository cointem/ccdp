package agent

import (
	"context"
	"path/filepath"

	"ccdp/internal/protocol"
)

// The typed runtime owns the command entry points below.  Keeping the small
// adapters together avoids reintroducing a second direct-control path for the
// older UI commands while the command protocol is rolled out.
func (a *Agent) applyQueryCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.Query == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "query payload is required")
	}
	return a.scheduleCommandOperationWithBusy(cmd, "query:"+string(cmd.Query.Kind), func(ctx context.Context) (string, error) {
		report, err := a.Query(ctx, cmd.Query.Kind)
		if err != nil {
			return "", err
		}
		return report.Text, nil
	}, false)
}

func (a *Agent) applySetWorkspaceCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.Workspace == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "workspace payload is required")
	}
	return a.scheduleCommandOperation(cmd, "cd", func(ctx context.Context) (string, error) {
		abs, err := filepath.Abs(cmd.Workspace.Path)
		if err != nil {
			return "", err
		}
		if err := a.setWorkspaceContext(ctx, abs); err != nil {
			return "", err
		}
		a.emitStatus("workspace → %s", abs)
		return "workspace → " + abs, nil
	})
}

func (a *Agent) applyReloadSettingsCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.Reload == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "reload payload is required")
	}
	return a.scheduleCommandOperation(cmd, "reload", func(ctx context.Context) (string, error) {
		a.mu.Lock()
		workspace := a.cfg.Workspace
		a.mu.Unlock()
		if err := a.reloadSettingsContext(ctx, workspace); err != nil {
			return "", err
		}
		a.emitStatus("settings reloaded from .ccdp/settings.json")
		return "settings reloaded from .ccdp/settings.json", nil
	})
}

func (a *Agent) applyTrustProjectCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.TrustProject == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "trust payload is required")
	}
	return a.scheduleCommandOperation(cmd, "trust", func(ctx context.Context) (string, error) {
		if err := a.trustProjectContext(ctx, cmd.TrustProject.Revoke); err != nil {
			return "", err
		}
		if cmd.TrustProject.Revoke {
			return "project trust revoked", nil
		}
		return "project trusted and executable settings reloaded", nil
	})
}

func (a *Agent) applyClearMemoryCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.ClearMemory == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "clear memory payload is required")
	}
	return a.scheduleCommandOperation(cmd, "memory", func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if err := a.ClearMemory(); err != nil {
			return "", err
		}
		return "memory cleared", nil
	})
}

func (a *Agent) applySaveSessionCommand(cmd protocol.Command) protocol.Receipt {
	if cmd.SaveSession == nil {
		return a.rejectedReceipt(cmd, protocol.ErrorInvalidCommand, "save payload is required")
	}
	return a.scheduleCommandOperationWithBusy(cmd, "save", func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if err := a.Save(); err != nil {
			return "", err
		}
		return "session saved", nil
	}, false)
}
