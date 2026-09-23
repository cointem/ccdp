package tui

func noticeText(m *Model) string {
	if notice, ok := m.latestNotice(); ok {
		return notice.Text
	}
	return ""
}
