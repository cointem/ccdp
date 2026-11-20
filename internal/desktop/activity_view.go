package desktop

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
)

// The action label is UI copy. Paths, queries, commands and tool output come
// from the recorded invocation and are kept verbatim in its details.
type activityAction struct {
	icon, verb, object string
}

var activityActions = map[string]activityAction{
	"read": {"book", "读取", "文件"}, "write": {"edit", "写入", "文件"}, "edit": {"edit", "编辑", "文件"},
	"ls": {"folder", "列出", "文件"}, "glob": {"search", "查找", "文件"}, "rg": {"search", "搜索", "文件内容"},
	"codenavigate": {"search", "查找", "代码符号"}, "bash": {"terminal", "运行", "命令"},
	"runcode": {"code", "运行", "代码"}, "process": {"terminal", "操作", "进程"},
	"webfetch":  {"book", "读取", "网页"},
	"readskill": {"skill", "读取", "技能"}, "readoutput": {"book", "读取", "工具输出"},
	"toolsearch": {"search", "查找", "工具"},
	"todowrite":  {"edit", "更新", "待办事项"}, "creategoal": {"new", "创建", "目标"},
	"getgoal": {"book", "查看", "目标"}, "updategoal": {"edit", "更新", "目标"},
	"spawnagent": {"chat", "启动", "子任务"}, "sendmessage": {"chat", "发送", "消息"},
	"followupagent": {"chat", "继续", "子任务"}, "waitagent": {"chat", "等待", "子任务"},
	"listagents": {"chat", "列出", "子任务"}, "readagent": {"book", "读取", "子任务结果"},
	"stopagent": {"stop", "停止", "子任务"}, "computer": {"code", "操作", "电脑"},
}

type activityArgs struct {
	FilePath                                         string `json:"file_path"`
	Path, Command, Pattern, Query, URL, Name, Action string
}

func activityStatus(status string) string {
	switch status {
	case "success", "succeeded", "completed", "complete", "done":
		return "成功"
	case "queued", "pending":
		return "等待执行"
	case "running", "streaming":
		return "进行中"
	case "error", "failed", "failure":
		return "失败"
	case "denied", "rejected":
		return "已拒绝"
	case "blocked":
		return "受阻"
	case "cancelled", "canceled", "stopped", "interrupted":
		return "已停止"
	case "resource_limit", "limit_reached":
		return "已达上限"
	default:
		return "结果未知"
	}
}

func toolState(item protocol.TranscriptItem) string {
	status := activityStatus(item.Status)
	p := item.Process
	if p == nil || !strings.EqualFold(item.Tool, "Bash") || (status != "成功" && status != "进行中") {
		return status
	}
	switch {
	case p.Status == "":
		return status
	case !p.Terminal():
		return "进行中"
	case p.ExitCode != nil && *p.ExitCode != 0:
		return fmt.Sprintf("退出码 %d", *p.ExitCode)
	case p.Status == "exited" && p.ExitCode != nil:
		return "成功"
	default:
		return activityStatus(p.Status)
	}
}

// Legacy file workers recorded the os.PathError as text. Use the missing
// component reported by the worker, which can be an ancestor of file_path.
func missingReadPath(item protocol.TranscriptItem) string {
	if !strings.EqualFold(item.Tool, "Read") || activityStatus(item.Status) != "失败" {
		return ""
	}
	line, ok := strings.CutSuffix(strings.TrimSpace(item.Text), ": no such file or directory")
	if !ok {
		return ""
	}
	for _, op := range []string{"lstat ", "stat ", "open "} {
		if _, path, ok := strings.Cut(line, ": "+op); ok {
			return path
		}
		if path, ok := strings.CutPrefix(line, op); ok {
			return path
		}
	}
	return ""
}

func activitySummary(item protocol.TranscriptItem) (glyph, label string) {
	switch item.Kind {
	case "thinking":
		return "thinking", "思考过程"
	case "assistant":
		return "chat", "进度说明"
	case "collaboration":
		return "agent", "子智能体报告"
	}
	tool := strings.ToLower(item.Tool)
	action, known := activityActions[tool]
	if !known {
		action = activityAction{"extensions", "调用", cmp.Or(item.Tool, "工具")}
	}
	var args activityArgs
	_ = json.Unmarshal(item.Args, &args)
	object := action.object
	verb := action.verb
	switch tool {
	case "read", "edit", "write", "codenavigate", "ls":
		path := args.FilePath
		if path == "" {
			path = args.Path
		}
		if path != "" && path != "." {
			object = filepath.Base(path)
			if tool == "ls" {
				object += " 中的文件"
			}
		}
	case "rg", "glob":
		if args.Pattern != "" {
			object = "「" + args.Pattern + "」"
			if args.Path != "" && args.Path != "." {
				verb = "在 " + filepath.Base(args.Path) + " 中" + verb
			}
		}
	case "webfetch":
		if args.URL != "" {
			object = args.URL
		}
	case "readskill":
		if args.Name != "" {
			object = args.Name
		}
	case "toolsearch":
		if query := cmp.Or(args.Name, args.Query); query != "" {
			object = "工具「" + query + "」"
		}
	case "process":
		switch args.Action {
		case "wait":
			verb = "等待"
		case "read":
			verb = "读取输出"
		case "write":
			verb = "发送输入"
		case "stop":
			verb = "停止"
		}
		if item.Process != nil && item.Process.Command != "" {
			object = item.Process.Command
		}
	}
	status := toolState(item)
	label = verb + " " + object
	if object == action.object || strings.HasPrefix(object, "「") {
		label = verb + object
	}
	switch status {
	case "成功":
		label = "已" + label
	case "进行中":
		label = "正在" + label
	case "等待执行":
		label = "等待" + label
	}
	if tool == "bash" {
		command := args.Command
		if command == "" && item.Process != nil {
			command = item.Process.Command
		}
		if command != "" {
			label = command
			switch status {
			case "进行中":
				label = "正在运行 " + command
			case "等待执行":
				label = "等待运行 " + command
			}
		}
	}
	if status != "成功" && status != "进行中" && status != "等待执行" {
		if missingReadPath(item) != "" {
			status = "路径不存在"
		}
		label += " · " + status
	}
	return action.icon, shorten(strings.Join(strings.Fields(label), " "), 180)
}

// Only the active row shows a chevron. Its hit area and keyboard focus target
// stay the same size when the pointer moves.
func activityButton(c *ui.Context, glyph, label string, open, running bool, toggle func()) {
	t := c.Theme()
	b := ui.ButtonBase(c).Label(label).Expanded(open).Cursor(ui.CursorPointer).WidthPercent(100).MinWidth(0).
		AlignItems(ui.Center).Justify(ui.Start).Gap(7).Padding(4, 2)
	active := b.Hovered() || b.FocusVisible()
	ink := t.TextMuted
	if active {
		ink = t.Text
	} else if running {
		// A slow pulse stays legible throughout. Keyframes settle at the
		// final color when the system requests reduced motion.
		phase := b.Keyframes("running", []ui.Keyframe{
			{Value: 0}, {At: 800 * time.Millisecond, Value: .72, Ease: ui.EaseInOut},
			{At: 1600 * time.Millisecond, Value: 0, Ease: ui.EaseInOut},
		}, ui.KeyframeOptions{Repeat: ui.RepeatForever})
		ink = ink.Mix(t.Text, phase)
	}
	b.Children(func() {
		if glyph != "" {
			ui.Icon(c, icons[glyph]).Size(14, 14).Shrink(0).TextColor(ink)
		}
		ui.Text(c, label).SingleLine().Shrink(1).MinWidth(0).FontSize(13).TextColor(ink)
		if active || open {
			chevron := "right"
			if open {
				chevron = "down"
			}
			ui.Icon(c, icons[chevron]).Size(11, 11).Shrink(0).TextColor(t.TextMuted)
		}
	}).OnClick(toggle)
}

func activityContents(item protocol.TranscriptItem, label string) (header, args, output string) {
	header = label
	args, output = string(item.Args), item.Text
	if item.Kind == "tool" {
		header = item.Tool
		if action, ok := activityActions[strings.ToLower(item.Tool)]; ok {
			header = action.verb + action.object
		}
		if header == "" {
			header = "工具调用"
		}
	}
	if strings.EqualFold(item.Tool, "Bash") {
		header = "终端"
		var fields map[string]json.RawMessage
		var command string
		if json.Unmarshal(item.Args, &fields) == nil && json.Unmarshal(fields["command"], &command) == nil {
			delete(fields, "command")
			args = ""
			if len(fields) > 0 {
				extra, _ := json.MarshalIndent(fields, "", "  ")
				args = string(extra)
			}
		}
		if p := item.Process; p != nil {
			if command == "" {
				command = p.Command
			}
			if p.Output != "" || p.Tail != "" || output == p.ProcessOutput.Text() {
				var parts []string
				for _, part := range []string{p.Output, p.Tail, p.Warning} {
					if part != "" {
						parts = append(parts, part)
					}
				}
				output = strings.Join(parts, "\n")
			}
		}
		if command != "" {
			if args != "" {
				args = "\n" + args
			}
			args = "$ " + command + args
		}
	} else if len(item.Args) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, item.Args, "", "  ") == nil {
			args = pretty.String()
		}
	}
	return header, args, output
}

func (a *app) activityDetail(c *ui.Context, conv *conversation, item protocol.TranscriptItem) {
	if a.agentOperation(c, conv, item) {
		return
	}
	glyph, label := activitySummary(item)
	if item.Kind == "tool" && toolState(item) == "成功" {
		switch strings.ToLower(item.Tool) {
		case "read", "edit", "write":
			var args activityArgs
			if json.Unmarshal(item.Args, &args) == nil && args.FilePath != "" {
				activityButton(c.Key(item.ID), glyph, label, false, false, func() { a.openFileActivity(conv, item, args.FilePath) })
				return
			}
		}
	}
	open := conv.expanded[item.ID]
	ui.Column(c.Key(item.ID)).Gap(4).Children(func() {
		activityButton(c, glyph, label, open, toolState(item) == "进行中", func() { conv.list.HoldPlace(); conv.expanded[item.ID] = !open })
		if !open {
			return
		}
		t := c.Theme()
		ui.Column(c).Margin(0, 2, 3, 20).Padding(9, 11).Gap(7).Background(t.Surface).Border(1, t.Border).Radius(8).Children(func() {
			header, args, output := activityContents(item, label)
			ui.Text(c, header).FontSize(11).TextColor(t.TextMuted)
			if args != "" || output != "" {
				ui.Scroll(c.Key("details")).MaxHeight(200).Gap(6).Children(func() {
					if args != "" {
						ui.Text(c, args).Selectable().Font("monospace").FontSize(12)
					}
					if output != "" {
						text := ui.Text(c, output).Selectable().FontSize(12)
						if item.Kind == "tool" {
							text.Font("monospace")
						}
					}
				})
			}
			truncated := item.Truncated || item.Process != nil && item.Process.Truncated
			if !truncated && item.Kind != "tool" {
				return
			}
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				if truncated {
					ui.Text(c, "输出已截断").FontSize(11).TextColor(t.TextMuted)
				}
				if item.Kind != "tool" {
					return
				}
				ui.Spacer(c)
				status := toolState(item)
				if status == "成功" {
					ui.Icon(c, icons["check"]).Size(12, 12).TextColor(t.TextMuted)
				}
				ui.Text(c, status).FontSize(11).TextColor(t.TextMuted)
			})
		})
	})
}
