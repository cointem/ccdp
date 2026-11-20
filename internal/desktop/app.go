package desktop

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"ccdp/internal/config"
	"ccdp/internal/protocol"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

type conversation struct {
	inspector inspectorState
	projection
	client                             protocol.SessionClient
	directory                          protocol.SessionDirectory
	rootID, title, draft               string
	draftModel, draftMode, draftEffort string // Explicit choices before the first message.
	configPending, configApplying      bool
	configModeChanged                  bool
	configError                        string
	pendingSettingsRev                 uint64 // Latest model or effort change acknowledged by Submit.
	createdAt, updatedAt               time.Time
	connected, sending, loading        bool
	submitted                          bool
	pending                            *protocol.Command
	history                            []protocol.TranscriptItem
	historyPrompt                      *protocol.TranscriptItem
	before                             uint64
	more                               bool
	list                               ui.ListState
	outlineActive                      string
	expanded                           map[string]bool
	questionID                         string
	answers                            []protocol.QuestionAnswer
}

// A runtime can be open before it has admitted the first input.
func (c *conversation) started() bool {
	return c.submitted || c.view.HistoryLength > 0 || len(c.view.PendingInputs) > 0 ||
		c.view.LastTurn != nil || c.view.Goal != nil
}

func (c *conversation) executing() bool {
	if c.view.Closing || c.view.Phase == protocol.PhaseClosed {
		return false
	}
	return c.view.Busy || slices.ContainsFunc(c.children, func(ch protocol.ChildSession) bool { return ch.Run.Active() })
}

func (c *conversation) inputConnected() bool {
	return c != nil && c.client != nil && c.connected && !c.view.Closing && c.view.Phase != protocol.PhaseClosed
}

func (a *app) canMessageChild(child *conversation) bool {
	root := a.conversations[child.rootID]
	if !root.inputConnected() || !child.inputConnected() {
		return false
	}
	info := childFor(root, child.view.SessionID, nil)
	// Capabilities and the conversation arrive on separate subscriptions.
	// Only apply a capability to the run that advertised it.
	return info != nil && child.view.RunID != "" && info.Run.ID == child.view.RunID &&
		(info.Capabilities.Send || info.Capabilities.Continue)
}

type app struct {
	host                      *host
	ctx                       context.Context
	cancel                    context.CancelFunc
	win                       *mygo.Window
	dispatch                  func(func())
	conversations             map[string]*conversation
	current                   string
	saved                     []savedSession
	workspace                 string
	configPath                string
	search                    string
	notice                    string
	opening                   bool
	closed                    bool
	settingsPage              *settingsPage
	projects                  *projectStore
	projectPage               *projectPage
	pluginsPage               *customizationPage
	afterDiscard              primaryDestination
	sidebar                   bool
	sidebarWidth, detailWidth float32
}

// Run opens the native client. Configuration, archives, approval policy and
// execution all remain owned by the same runtime the terminal client uses.
func Run(cfg config.Config, configPath, resume string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &app{host: newHost(cfg), ctx: ctx, cancel: cancel,
		workspace: cfg.Workspace, configPath: configPath, sidebar: true,
		conversations: make(map[string]*conversation)}
	var err error
	a.projects, err = loadProjectStore(projectStorePath())
	if err == nil && a.workspace != "" {
		err = a.projects.update(a.workspace, nil)
	}
	if err != nil {
		a.notice = "无法读取或保存项目列表：" + err.Error()
	}
	defer a.host.close()
	mygo.App.SetName("ccdp")
	mygo.App.SetVersion("0.2.0")
	mygo.App.WhenReady(func() {
		a.win = mygo.NewWindow(mygo.WindowOptions{
			Title: "ccdp", Width: 1180, Height: 800, MinWidth: 780, MinHeight: 580,
			TitleBarStyle: mygo.TitleBarHidden, TrafficLightPosition: &mygo.Point{X: 16, Y: 15},
			StateKey: "ccdp-main", Content: ui.View(a.view),
		})
		a.dispatch = func(fn func()) {
			a.win.Update(func() {
				if !a.closed {
					fn()
				}
			})
		}
		a.installMenu()
		a.refreshSaved()
		if resume != "" {
			a.openSession(a.workspace, resume)
		}
	})
	// Let the main loop continue while runtime.Close joins tools, watchers and
	// journal writers. Do not leave a process executing after closing the app.
	finished := false
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) {
		if finished || e.SessionEnding {
			return
		}
		e.PreventDefault()
		if a.closed {
			return
		}
		a.closed = true
		cancel()
		go func() {
			a.host.close()
			mygo.RunOnMain(func() { finished = true; mygo.App.Quit() })
		}()
	})
	return mygo.App.Run()
}

func (a *app) installMenu() {
	mygo.App.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
		{Role: mygo.RoleAppMenu},
		{Label: "文件", Submenu: []*mygo.MenuItem{
			{Label: "新会话", Accelerator: "CmdOrCtrl+N", Click: func(*mygo.MenuItem, *mygo.Window) { a.win.Update(a.newConversation) }},
			{Label: "打开项目…", Accelerator: "CmdOrCtrl+O", Click: func(*mygo.MenuItem, *mygo.Window) { a.chooseWorkspace() }},
			mygo.Separator(), {Role: mygo.RoleClose},
		}},
		{Role: mygo.RoleEditMenu},
		{Label: "视图", Submenu: []*mygo.MenuItem{
			{Label: "显示 / 隐藏侧栏", Accelerator: "CmdOrCtrl+B", Click: func(*mygo.MenuItem, *mygo.Window) {
				a.win.Update(func() {
					if a.showingChat() {
						a.sidebar = !a.sidebar
					}
				})
			}},
			{Role: mygo.RoleToggleFullScreen},
		}},
		{Role: mygo.RoleWindowMenu},
	}))
}

func (a *app) active() *conversation { return a.conversations[a.current] }

func (a *app) applySavedConfig(saved config.Config) {
	cfg := a.host.settings.Load().Clone()
	oldMode := cfg.PermissionMode
	cfg.ApplyUserPreferences(saved, true)
	a.host.setConfig(cfg)
	for id, conv := range a.conversations {
		if conv.client == nil {
			a.updateDraftSettings(conv)
		} else if conv.rootID == id {
			conv.configModeChanged = conv.configModeChanged || oldMode != cfg.PermissionMode
			a.refreshUserPreferences(conv)
		}
	}
}

func (a *app) refreshUserPreferences(conv *conversation) {
	if conv.configApplying {
		conv.configPending = true
		return
	}
	conv.configApplying, conv.configPending, conv.configError = true, false, ""
	cmd := protocol.Command{ID: protocol.CommandID("desktop-" + rand.Text()), SessionID: conv.view.SessionID,
		Type: protocol.CommandReloadSettings, Reload: &protocol.ReloadCommand{UserPreferences: true, PermissionModeChanged: conv.configModeChanged}}
	go func() {
		receipt, err := conv.client.Submit(a.ctx, cmd)
		a.dispatch(func() {
			conv.configApplying = false
			if conv.configPending {
				a.refreshUserPreferences(conv)
				return
			}
			switch {
			case err != nil:
				conv.configError = err.Error()
			case receipt.Rejected() && receipt.Error != nil:
				conv.configError = receipt.Error.Message
			case !receipt.Applied():
				conv.configError = "会话未确认配置更新"
			default:
				conv.configModeChanged = false
			}
		})
	}()
}

func (a *app) selectConversation(id string) {
	if previous := a.active(); a.current != id && previous != nil && previous.client == nil && strings.TrimSpace(previous.draft) == "" {
		delete(a.conversations, a.current)
	}
	a.current = id
	if conv := a.active(); conv != nil && conv.client == nil {
		a.updateDraftSettings(conv)
	}
}

func (a *app) updateDraftSettings(conv *conversation) {
	cfg := a.host.settings.Load()
	settings := &conv.view.Settings
	model := conv.draftModel
	if !slices.Contains(cfg.models, model) {
		conv.draftModel = ""
		model = cfg.Model
	}
	settings.Model.Model = model
	settings.ReasoningEfforts = cfg.ReasoningEffortsFor(model)
	settings.ContextWindow = cfg.ContextWindowFor(model)
	if !slices.Contains(settings.ReasoningEfforts, conv.draftEffort) {
		conv.draftEffort = ""
	}
	configured := cfg.ReasoningEffortFor(model)
	if conv.draftEffort == "" {
		conv.draftEffort = compatibleEffort(settings.ReasoningEfforts, configured)
	}
	settings.ReasoningEffort = configured
	if conv.draftEffort != "" {
		settings.ReasoningEffort = conv.draftEffort
	}
	settings.ReasoningEffortSupported = slices.Contains(settings.ReasoningEfforts, settings.ReasoningEffort)
	if conv.draftMode != "" {
		settings.Permission.Mode = conv.draftMode
	} else {
		settings.Permission.Mode = a.host.draftPermission(settings.Workspace)
	}
}

func (a *app) newConversation() {
	if a.opening || a.closed || !a.showingChat() {
		return
	}
	// A draft has no runtime until it is first submitted.
	id := "draft-" + rand.Text()
	now := time.Now()
	a.conversations[id] = &conversation{title: "新会话", createdAt: now, updatedAt: now, expanded: make(map[string]bool),
		projection: projection{view: protocol.SessionView{Settings: protocol.SettingsSnapshot{
			Workspace: a.workspace,
		}}}}
	a.selectConversation(id)
}

func (a *app) chooseWorkspace() {
	if a.opening || a.closed || !a.showingChat() {
		return
	}
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Title: "选择项目文件夹", Directory: true, DefaultPath: a.workspace})
	if err != nil {
		a.notice = err.Error()
	}
	if len(paths) > 0 {
		a.openProjectForChat(paths[0], a.active())
	}
	if a.win != nil {
		a.win.Invalidate()
	}
}

func (a *app) openProjectForChat(path string, draft *conversation) {
	if a.opening || a.closed || !a.showingChat() {
		return
	}
	path, err := config.NormalizeProjectRoot(path)
	if err != nil {
		a.notice = "无法打开项目：" + err.Error()
		return
	}
	if err := a.projects.update(path, func(p *projectMetadata) { p.Collapsed = false }); err != nil {
		a.notice = "已打开项目，但无法保存到侧栏：" + err.Error()
	}
	a.workspace = path
	if draft != nil && draft.client == nil && draft.view.Settings.Workspace == "" {
		draft.view.Settings.Workspace = path
		a.updateDraftSettings(draft)
		return
	}
	a.newConversation()
}

func (a *app) refreshSaved() {
	go func() {
		saved, err := a.host.saved()
		a.dispatch(func() {
			a.saved = saved
			if err != nil {
				a.notice = err.Error()
			}
		})
	}()
}

func (a *app) openSession(workspace, id string) {
	if a.opening || a.closed {
		return
	}
	if cached := a.conversations[id]; cached != nil {
		if root := a.conversations[cached.rootID]; root != nil && root != cached {
			a.selectConversation(root.rootID)
			a.workspace = root.view.Settings.Workspace
			a.inspectAgent(root, agentDetail{id: cached.view.SessionID})
			return
		}
		a.selectConversation(id)
		a.workspace = cached.view.Settings.Workspace
		return
	}
	a.opening = true
	a.notice = ""
	go func() {
		runtime, snapshot, err := a.host.session(a.ctx, workspace, id, draftOptions{})
		visited := map[string]bool{id: true}
		for {
			var parent agentParent
			if !errors.As(err, &parent) || visited[string(parent)] {
				break
			}
			visited[string(parent)] = true
			runtime, snapshot, err = a.host.session(a.ctx, workspace, string(parent), draftOptions{})
		}
		a.dispatch(func() {
			a.opening = false
			if err != nil {
				a.notice = err.Error()
				return
			}
			conv := a.conversations[snapshot.SessionID.String()]
			if conv == nil {
				conv = a.attach(runtime, runtime.Sessions(), snapshot, snapshot.SessionID.String())
			}
			a.selectConversation(snapshot.SessionID.String())
			a.workspace = snapshot.Settings.Workspace
			if id != "" {
				for _, saved := range a.saved {
					if saved.ID == snapshot.SessionID.String() {
						conv.title = saved.Title
						conv.createdAt, conv.updatedAt = saved.CreatedAt, saved.UpdatedAt
						break
					}
				}
				if id != snapshot.SessionID.String() {
					a.inspectAgent(conv, agentDetail{id: protocol.SessionID(id)})
				}
			}
		})
	}()
}

func (a *app) attach(client protocol.SessionClient, directory protocol.SessionDirectory, snapshot protocol.SessionView, root string) *conversation {
	conv := &conversation{projection: projection{view: snapshot}, client: client, directory: directory,
		rootID: root, title: sessionTitle(snapshot), expanded: make(map[string]bool)}
	conv.list.FollowEnd = true
	conv.list.FollowAnimation = &ui.Smooth
	a.conversations[snapshot.SessionID.String()] = conv
	go a.observe(conv)
	return conv
}

func (a *app) observe(conv *conversation) {
	// Reattachment always starts with an authoritative snapshot. It never
	// resubmits commands; an uncertain submission retains its original ID.
	delay := 200 * time.Millisecond
	for a.ctx.Err() == nil {
		sub, err := conv.client.Watch(a.ctx, protocol.Cursor{})
		if err == nil {
			delay = 200 * time.Millisecond
			if conv.directory != nil {
				children, listErr := conv.directory.ListChildren(a.ctx)
				if listErr == nil {
					a.dispatch(func() {
						conv.children = children
					})
				}
			}
			err = a.consume(conv, sub)
			_ = sub.Close()
		}
		if a.ctx.Err() != nil {
			return
		}
		message := "会话连接中断，正在重新连接…"
		if err != nil {
			message += " " + err.Error()
		}
		a.dispatch(func() { conv.connected = false; conv.notice = message })
		timer := time.NewTimer(delay)
		select {
		case <-a.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, 5*time.Second)
	}
}

func (a *app) consume(conv *conversation, sub protocol.Subscription) error {
	for {
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		case update, ok := <-sub.Updates():
			if !ok || update.Type == protocol.UpdateResyncRequired {
				return nil
			}
			a.dispatch(func() {
				previousGeneration := conv.view.Revision.ViewGeneration
				wasBusy := conv.view.Busy
				conv.apply(update)
				if a.current == conv.view.SessionID.String() {
					a.workspace = conv.view.Settings.Workspace
				}
				if update.Snapshot != nil {
					conv.connected = true
					conv.notice = ""
				}
				if conv.view.Revision.ViewGeneration != previousGeneration {
					conv.history, conv.historyPrompt, conv.before, conv.more = nil, nil, 0, false
				}
				if conv.title == "新会话" {
					conv.title = sessionTitle(conv.view)
				}
				if wasBusy && !conv.view.Busy {
					a.refreshSaved()
				}
			})
		}
	}
}

func (a *app) send(conv *conversation) {
	if conv.sending || conv.configApplying || conv.configError != "" || a.opening || a.closed {
		return
	}
	if conv.rootID != "" && conv.rootID != conv.view.SessionID.String() && !a.canMessageChild(conv) {
		return
	}
	if conv.client == nil {
		if strings.TrimSpace(conv.draft) == "" {
			return
		}
		workspace := a.workspace
		if workspace == "" {
			a.notice = "请先打开一个项目文件夹"
			return
		}
		a.opening = true
		oldID := a.current
		text := conv.draft
		opts := draftOptions{model: conv.view.Settings.Model.Model, mode: conv.draftMode, effort: conv.draftEffort}
		go func() {
			runtime, snapshot, err := a.host.session(a.ctx, workspace, "", opts)
			a.dispatch(func() {
				a.opening = false
				if err != nil {
					a.notice = err.Error()
					return
				}
				opened := a.attach(runtime, runtime.Sessions(), snapshot, snapshot.SessionID.String())
				opened.createdAt = time.Now()
				opened.draft = conv.draft
				id := "desktop-" + rand.Text()
				command := protocol.NewSubmitInput(protocol.CommandID(id), snapshot.SessionID, protocol.InputID(id), text, protocol.InputSteer)
				opened.pending = &command
				delete(a.conversations, oldID)
				a.selectConversation(snapshot.SessionID.String())
				a.send(opened)
			})
		}()
		return
	}
	if conv.pending == nil {
		if strings.TrimSpace(conv.draft) == "" {
			return
		}
		id := "desktop-" + rand.Text()
		cmd := protocol.NewSubmitInput(protocol.CommandID(id), conv.view.SessionID, protocol.InputID(id), conv.draft, protocol.InputSteer)
		cmd.ExpectedRunID = conv.view.RunID
		conv.pending = &cmd
	}
	command := *conv.pending
	// When replying to an older report, follow the live child after admission.
	// Capture this selection so a delayed receipt cannot switch a newer report.
	var report *agentDetail
	if root := a.conversations[conv.rootID]; root != nil && root != conv {
		if page := root.inspector.current; page != nil && page.agent != nil && page.agent.id == conv.view.SessionID && page.agent.isReport {
			report = page.agent
		}
	}
	conv.sending = true
	conv.notice = ""
	go func() {
		receipt, err := conv.client.Submit(a.ctx, command)
		a.dispatch(func() {
			conv.sending = false
			if errors.Is(err, protocol.ErrStaleRun) {
				conv.pending = nil
				conv.notice = "子智能体已进入下一轮，请重新发送。"
				return
			}
			if err != nil {
				conv.notice = "发送状态未确认；点击重试将使用同一请求，不会重复提交。" + err.Error()
				return
			}
			conv.pending = nil
			if receipt.Rejected() {
				conv.notice = "发送被拒绝"
				if receipt.Error != nil {
					conv.notice = receipt.Error.Message
				}
				return
			}
			conv.submitted = true
			if report != nil {
				report.isReport = false
			}
			conv.updatedAt = time.Now()
			if conv.title == "新会话" {
				conv.title = shorten(strings.Join(strings.Fields(command.Input.Text), " "), 40)
			}
			if conv.draft == command.Input.Text {
				conv.draft = ""
			}
			conv.list.ScrollToEnd()
		})
	}()
}

func (a *app) command(conv *conversation, command protocol.Command) {
	if conv.client == nil || conv.sending {
		return
	}
	command.ID = protocol.CommandID("desktop-" + rand.Text())
	command.SessionID = conv.view.SessionID
	command.ExpectedRunID = conv.view.RunID
	conv.sending = true
	go func() {
		receipt, err := conv.client.Submit(a.ctx, command)
		a.dispatch(func() {
			conv.sending = false
			if err != nil {
				conv.notice = err.Error()
				return
			}
			if receipt.Rejected() && receipt.Error != nil {
				conv.notice = receipt.Error.Message
			}
			if receipt.Applied() && (command.Type == protocol.CommandSetModel || command.Type == protocol.CommandSetGeneration) {
				conv.pendingSettingsRev = receipt.Revision.SettingsRev
			}
		})
	}()
}

func (a *app) loadHistory(conv *conversation) {
	if conv.loading || conv.directory == nil {
		return
	}
	conv.loading = true
	before, generation, id := conv.before, conv.view.Revision.ViewGeneration, conv.view.SessionID
	go func() {
		page, err := conv.directory.ReadTranscript(a.ctx, id, before, 200)
		a.dispatch(func() {
			conv.loading = false
			if err != nil {
				conv.notice = err.Error()
				return
			}
			if generation != conv.view.Revision.ViewGeneration {
				return
			}
			conv.history = mergeTranscript(page.Items, conv.history)
			conv.before, conv.more, conv.historyPrompt = page.Before, page.More, page.Prompt
		})
	}()
}

func mergeTranscript(older, newer []protocol.TranscriptItem) []protocol.TranscriptItem {
	if len(older) == 0 {
		return newer
	}
	p := projection{view: protocol.SessionView{Transcript: slices.Clone(older)}}
	for _, item := range newer {
		p.upsert(item)
	}
	return p.view.Transcript
}

func (a *app) allSessionEntries() []savedSession {
	entries := slices.Clone(a.saved)
	for id, conv := range a.conversations {
		if conv.client != nil && conv.rootID != id {
			continue
		}
		i := slices.IndexFunc(entries, func(s savedSession) bool { return s.ID == id })
		entry := savedSession{ID: id, Title: conv.title, Workspace: conv.view.Settings.Workspace, CreatedAt: conv.createdAt, UpdatedAt: conv.updatedAt}
		if i < 0 && !conv.started() {
			draft := strings.TrimSpace(conv.draft)
			if draft == "" && conv.pending != nil {
				draft = conv.pending.Input.Text
			}
			if draft == "" {
				continue
			}
			entry.Draft = true
			entry.Title = "草稿 · " + shorten(strings.Join(strings.Fields(draft), " "), 30)
		}
		if i >= 0 {
			saved := entries[i]
			if !saved.CreatedAt.IsZero() {
				entry.CreatedAt = saved.CreatedAt
			}
			if saved.UpdatedAt.After(entry.UpdatedAt) {
				entry.UpdatedAt = saved.UpdatedAt
			}
			entries[i] = entry
		} else {
			entries = append(entries, entry)
		}
	}
	slices.SortFunc(entries, func(a, b savedSession) int {
		if order := b.creationTime().Compare(a.creationTime()); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	return entries
}

func (s savedSession) creationTime() time.Time {
	if s.CreatedAt.IsZero() {
		return s.UpdatedAt
	}
	return s.CreatedAt
}

func projectName(path string) string {
	if path == "" {
		return "选择项目"
	}
	return filepath.Base(path)
}
