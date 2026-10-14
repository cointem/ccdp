package agent

import (
	"strings"
)

func turnIDOrLegacy(turnID string) string {
	if strings.TrimSpace(turnID) == "" {
		return "turn-legacy"
	}
	return turnID
}
