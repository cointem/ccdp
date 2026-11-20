// Package desktop is the native macOS client of the shared session protocol.
package desktop

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccdp/internal/agent"
	"ccdp/internal/config"
	"ccdp/internal/protocol"
	"ccdp/internal/session/archive"
)

// Runtime handles live here, never in the renderer. Keeping an opened root
// alive lets another conversation run while the user navigates elsewhere.
type runtimeSession interface {
	protocol.SessionClient
	Sessions() protocol.SessionDirectory
	Close()
}

type sessionFactory func(config.Config, string) (runtimeSession, error)

type agentParent string

func (p agentParent) Error() string { return "子智能体属于主会话 " + string(p) }

type draftOptions struct{ model, mode, effort string }

type savedSession struct {
	ID, Title, Workspace string
	CreatedAt, UpdatedAt time.Time
	Draft                bool
}

type host struct {
	mu       sync.Mutex
	settings atomic.Pointer[hostSettings]
	open     sessionFactory
	closed   bool
	entries  map[string]runtimeSession
}

type hostSettings struct {
	config.Config
	models []string
}

func pickerModel(provider, model string) string {
	if provider == "default" || strings.HasPrefix(model, provider+"/") {
		return model
	}
	return provider + "/" + model
}

func (h *host) setConfig(cfg config.Config) {
	models := []string{cfg.Model}
	for id, provider := range cfg.Providers {
		if provider.ModelConfigs != nil {
			for name := range provider.ModelConfigs {
				models = append(models, id+"/"+name)
			}
		} else {
			for _, name := range provider.Models {
				models = append(models, pickerModel(id, name))
			}
		}
	}
	slices.Sort(models)
	h.settings.Store(&hostSettings{Config: cfg, models: slices.Compact(models)})
}

func newHost(cfg config.Config) *host {
	h := &host{entries: make(map[string]runtimeSession), open: func(cfg config.Config, id string) (runtimeSession, error) {
		var ag *agent.Agent
		var err error
		if id == "" {
			ag, err = agent.New(&cfg)
		} else {
			var snap *archive.Snapshot
			snap, err = archive.Load(cfg.SessionDir, id)
			if err == nil {
				if parent := snap.AgentParent(); parent != "" {
					return nil, agentParent(parent)
				}
				ag, err = agent.Resume(&cfg, snap)
			}
		}
		if err != nil {
			return nil, err
		}
		ag.SetChildInteraction(true)
		return ag, nil
	}}
	h.setConfig(cfg)
	return h
}

func workspacePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("请先选择项目文件夹")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("项目路径不是文件夹：%s", path)
	}
	return path, nil
}

func (h *host) draftPermission(workspace string) string {
	if workspace == "" {
		return h.settings.Load().PermissionMode
	}
	cfg := h.settings.Load().Clone()
	cfg.Workspace = workspace
	project, err := config.LoadProjectSettings(workspace)
	if err != nil {
		return ""
	}
	config.ApplyProjectSettings(&cfg, &project)
	if err := cfg.ApplyProjectPermission(); err != nil {
		return ""
	}
	return cfg.PermissionMode
}

func modeCommand(view protocol.SessionView, mode string) protocol.Command {
	policy := view.Settings.Permission
	policy.Mode = mode
	return protocol.Command{Type: protocol.CommandSetPermissionPolicy, ExpectedRevision: view.Revision.LogSeq,
		PermissionPolicy: &protocol.SetPermissionPolicy{Policy: policy}}
}

func (h *host) session(ctx context.Context, workspace, id string, opts draftOptions) (runtimeSession, protocol.SessionView, error) {
	// Serialize creation and shutdown, including opening the same saved log.
	// All calls run outside the UI thread.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, protocol.SessionView{}, errors.New("桌面端已关闭")
	}
	if err := ctx.Err(); err != nil {
		return nil, protocol.SessionView{}, err
	}
	if existing := h.entries[id]; existing != nil {
		view, err := existing.Snapshot(ctx)
		return existing, view, err
	}
	cfg := h.settings.Load().Clone()
	if id == "" {
		path, err := workspacePath(workspace)
		if err != nil {
			return nil, protocol.SessionView{}, err
		}
		// Use the same explicit workspace precedence as the CLI's -d flag.
		if err := cfg.ApplyCLIOverride("workspace", path); err != nil {
			return nil, protocol.SessionView{}, err
		}
		if opts.model != "" && opts.model != cfg.Model {
			if err := cfg.ApplyCLIOverride("model", opts.model); err != nil {
				return nil, protocol.SessionView{}, err
			}
		}
		if opts.effort != "" {
			if !cfg.SupportsReasoningEffort(cfg.Model, opts.effort) {
				return nil, protocol.SessionView{}, fmt.Errorf("%s 不支持推理档位 %s", cfg.Model, opts.effort)
			}
			if err := cfg.ApplyCLIOverride("reasoning_effort", opts.effort); err != nil {
				return nil, protocol.SessionView{}, err
			}
		}
	}
	cfg.SessionID = ""
	runtime, err := h.open(cfg, id)
	if err != nil {
		return nil, protocol.SessionView{}, err
	}
	view, err := runtime.Snapshot(ctx)
	if err == nil && id == "" && view.SessionID != "" && opts.mode != "" && view.Settings.Permission.Mode != opts.mode {
		cmd := modeCommand(view, opts.mode)
		cmd.ID, cmd.SessionID = protocol.CommandID("desktop-"+rand.Text()), view.SessionID
		var receipt protocol.Receipt
		receipt, err = runtime.Submit(ctx, cmd)
		if err == nil && !receipt.Applied() {
			err = errors.New("无法应用所选权限模式")
			if receipt.Error != nil {
				err = receipt.Error
			}
		}
		if err == nil {
			view, err = runtime.Snapshot(ctx)
			if err == nil && view.Settings.Permission.Mode != opts.mode {
				err = errors.New("所选权限模式尚未生效")
			}
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil || view.SessionID == "" {
		runtime.Close()
		if err == nil {
			err = errors.New("会话没有返回有效标识")
		}
		return nil, protocol.SessionView{}, err
	}
	h.entries[view.SessionID.String()] = runtime
	return runtime, view, nil
}

func (h *host) saved() ([]savedSession, error) {
	snapshots, _, err := archive.List(h.settings.Load().SessionDir)
	if err != nil {
		return nil, err
	}
	result := make([]savedSession, 0, len(snapshots))
	for _, snap := range snapshots {
		if snap.AgentParent() != "" {
			continue
		}
		if len(snap.History) == 0 && len(snap.PendingInputs) == 0 && snap.TurnSeq == 0 && snap.Goal == nil {
			continue
		}
		title := snap.Title
		for _, input := range snap.PendingInputs {
			if title != "" {
				break
			}
			if input.Strategy != protocol.InputMessage {
				title = shorten(strings.Join(strings.Fields(input.Text), " "), 60)
			}
		}
		if title == "" {
			title = "新会话"
		}
		result = append(result, savedSession{ID: snap.ID, Title: title, Workspace: snap.Workspace, CreatedAt: snap.CreatedAt, UpdatedAt: snap.UpdatedAt})
	}
	return result, nil
}

func (h *host) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	entries := h.entries
	h.entries = nil
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, runtime := range entries {
		wg.Add(1)
		go func() { defer wg.Done(); runtime.Close() }()
	}
	wg.Wait()
}
