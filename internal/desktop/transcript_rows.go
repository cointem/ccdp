package desktop

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ccdp/internal/protocol"
	"github.com/egoist/mygo/ui"
)

type activityGroup struct {
	key, glyph, label string
	running           bool
}

type transcriptRow struct {
	spawns       []protocol.TranscriptItem
	missingReads []protocol.TranscriptItem
	missingPath  string
	item         *protocol.TranscriptItem
	group        *activityGroup
	detail       bool
}

func (r transcriptRow) key() string {
	if len(r.missingReads) > 0 {
		return "missing-reads:" + r.missingReads[0].ID
	}
	if len(r.spawns) > 0 {
		return "agents:" + r.spawns[0].ID
	}
	if r.group != nil {
		return r.group.key
	}
	return r.item.ID
}

func sameTurn(a, b protocol.TurnID) bool {
	return strings.TrimPrefix(string(a), "turn-") == strings.TrimPrefix(string(b), "turn-")
}

func workItem(item protocol.TranscriptItem) bool {
	switch item.Kind {
	case "tool":
		if isSpawn(item) {
			return true
		}
		switch toolState(item) {
		case "成功", "进行中", "等待执行":
			return true
		default:
			return false
		}
	case "thinking", "assistant", "turn_summary":
		return true
	case "collaboration":
		msg, ok := protocol.DecodeCollaboration(item.Text)
		return ok && msg.Kind == "final_result" && childStatus(msg.Outcome) == "已完成"
	}
	return false
}

func isSpawn(item protocol.TranscriptItem) bool {
	return item.Kind == "tool" && strings.EqualFold(item.Tool, "SpawnAgent")
}

func quietWait(item protocol.TranscriptItem) bool {
	if item.Kind != "tool" || !strings.EqualFold(item.Tool, "WaitAgent") {
		return false
	}
	switch toolState(item) {
	case "进行中", "等待执行":
		return true
	case "成功":
		var result struct{ Reason string }
		if json.Unmarshal([]byte(item.Text), &result) == nil {
			switch result.Reason {
			case "collaboration", "user_input", "no_pending_work":
				return true
			}
		}
	}
	return false
}

// The work leading up to an answer has one disclosure. Keep all assistant
// segments after the last tool call visible, even when reasoning is interleaved.
// User messages, errors, and changes of turn always start a new span.
func transcriptRows(items []protocol.TranscriptItem, expanded map[string]bool) []transcriptRow {
	rows := make([]transcriptRow, 0, len(items))
	for start := 0; start < len(items); {
		if !workItem(items[start]) {
			if missing := missingReadPath(items[start]); missing != "" {
				end := start + 1
				for end < len(items) && sameTurn(items[start].TurnID, items[end].TurnID) && missingReadPath(items[end]) == missing {
					end++
				}
				if end > start+1 {
					row := transcriptRow{missingReads: items[start:end], missingPath: missing}
					rows = append(rows, row)
					if expanded[row.key()] {
						for i := start; i < end; i++ {
							rows = append(rows, transcriptRow{item: &items[i], detail: true})
						}
					}
					start = end
					continue
				}
			}
			rows = append(rows, transcriptRow{item: &items[start]})
			start++
			continue
		}
		end := start
		lastTool := -1
		var summary *protocol.TranscriptItem
		turn := items[start].TurnID
		for end < len(items) && workItem(items[end]) {
			item := &items[end]
			// A mailbox delivery is recorded on arrival, before it is assigned
			// to a turn. Keep those notices with the work on either side.
			if item.Kind != "collaboration" || item.TurnID != "" {
				if turn == "" && items[start].Kind == "collaboration" {
					turn = item.TurnID
				}
				if !sameTurn(turn, item.TurnID) {
					break
				}
			}
			end++
			if item.Kind == "tool" {
				lastTool = end - 1
			}
			if item.Kind == "turn_summary" {
				summary = item
				break
			}
		}
		isDetail := func(i int) bool {
			return i < lastTool || items[i].Kind == "tool" || items[i].Kind == "thinking" || items[i].Kind == "collaboration"
		}
		count, last, lastToolDetail, live := 0, start, -1, -1
		for i := start; i < end; i++ {
			item := items[i]
			if !isDetail(i) || isSpawn(item) || quietWait(item) {
				continue
			}
			count++
			last = i
			if item.Kind == "tool" {
				lastToolDetail = i
				if toolState(item) == "进行中" {
					live = i
				}
			} else if item.Kind == "thinking" && item.Status == "streaming" && (live < 0 || items[live].Kind == "thinking") {
				live = i
			}
		}
		open := false
		if count > 0 {
			// Anchor to the surrounding turn and boundary. Streamed work can
			// acquire a new item ID when its durable record is committed.
			key := "activity:" + string(items[start].TurnID)
			if start > 0 {
				key += "/" + items[start-1].ID
			}
			open = expanded[key]
			g := &activityGroup{key: key}
			switch {
			case live >= 0 && !open:
				g.glyph, g.label = activitySummary(items[live])
				g.running = true
				if items[live].Kind == "thinking" {
					g.label = "思考中"
				}
			case summary != nil && summary.DurationMs > 0:
				g.label = "用时 " + elapsedLabel(summary.DurationMs)
			case open:
				g.label = fmt.Sprintf("执行记录 · %d 项", count)
			default:
				if lastToolDetail >= 0 {
					last = lastToolDetail
				}
				g.glyph, g.label = activitySummary(items[last])
			}
			rows = append(rows, transcriptRow{group: g})
		}
		for i := start; i < end; i++ {
			item := &items[i]
			if isSpawn(*item) {
				first := i
				for i+1 < end && isSpawn(items[i+1]) {
					i++
				}
				rows = append(rows, transcriptRow{spawns: items[first : i+1]})
				continue
			}
			if quietWait(*item) {
				continue
			}
			if item == summary && activityStatus(summary.Status) == "成功" && (count > 0 || summary.DurationMs == 0) {
				continue
			}
			detail := isDetail(i)
			if !detail || open {
				rows = append(rows, transcriptRow{item: item, detail: detail})
			}
		}
		start = end
	}
	return rows
}

func elapsedLabel(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return "不到 1 秒"
	}
	seconds := int64(d.Round(time.Second) / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%d 秒", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%d 分 %d 秒", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", seconds/3600, seconds%3600/60)
}

func (a *app) transcriptRow(c *ui.Context, conv *conversation, row transcriptRow) {
	if len(row.missingReads) > 0 {
		path := row.missingPath
		if rel, err := projectRelativePath(conv.view.Settings.Workspace, path); err == nil {
			path = rel
		}
		key := row.key()
		label := fmt.Sprintf("%d 次读取失败 · 找不到 %s", len(row.missingReads), path)
		activityButton(c.Key(key), "book", label, conv.expanded[key], false, func() {
			conv.list.HoldPlace()
			conv.expanded[key] = !conv.expanded[key]
		})
	} else if len(row.spawns) != 0 {
		a.agentGroup(c.Key(row.key()), conv, row.spawns)
	} else if group := row.group; group != nil {
		open := conv.expanded[group.key]
		activityButton(c.Key(group.key), group.glyph, group.label, open, group.running, func() { conv.list.HoldPlace(); conv.expanded[group.key] = !open })
	} else if row.detail {
		ui.Column(c).Margin(0, 0, 0, 6).Padding(0, 0, 0, 12).BorderWidth(0, 0, 0, 1).BorderColor(c.Theme().Border).Children(func() {
			if row.item.Kind == "collaboration" {
				a.collaborationRow(c, conv, *row.item)
			} else {
				a.activityDetail(c, conv, *row.item)
			}
		})
	} else {
		ui.Column(c).Padding(8, 0).Children(func() { a.message(c, conv, *row.item) })
	}
}
