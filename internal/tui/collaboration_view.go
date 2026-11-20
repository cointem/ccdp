package tui

import (
	"ccdp/internal/protocol"
	"fmt"
)

func renderCollaborationCell(text string) string {
	message, ok := protocol.DecodeCollaboration(text)
	if !ok || message.Kind != "final_result" {
		return ""
	}
	name := displayLabel(message.Name)
	if name == "" {
		name = shortID(displayLabel(string(message.AgentID)))
	}
	_, _, label := agentMarkerStatus(message.Outcome)
	if label == "Done" {
		label = "已完成"
	}
	return styleStatus.Render(fmt.Sprintf("· %s：%s · 结果已交付", name, label))
}
