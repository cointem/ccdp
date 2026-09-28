package tui

import (
	"ccdp/internal/protocol"
	"fmt"
)

func renderCollaborationCell(text string, width int) string {
	message, ok := protocol.DecodeCollaboration(text)
	if !ok {
		return styleStatus.Render("· 协作消息") + "\n" + renderAssistantCell(text, width)
	}
	name := message.Name
	if name == "" {
		name = shortID(string(message.AgentID))
	}
	if message.Kind == "final_result" {
		_, _, label := agentMarkerStatus(message.Outcome)
		if label == "Done" {
			label = "已完成"
		}
		return styleStatus.Render(fmt.Sprintf("· %s：%s · 结果已交付", name, label))
	}
	return styleStatus.Render("· "+name+" 的消息") + "\n" + renderAssistantCell(message.Text, width)
}
