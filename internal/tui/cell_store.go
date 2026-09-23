package tui

// CellStore owns ordered protocol source and local reports. Views and terminal
// history consume projections of these cells; neither is a source of truth.
type CellStore struct {
	positions      map[string]cellPosition
	items          []historyCell
	confirmedItems []historyCell
	reports        []historyCell
	sequence       uint64
	versions       map[string]cellSourceVersion
}
type cellSourceVersion struct {
	kind, text, status, name, args string
	truncated                      bool
	revision                       uint64
}

type cellPosition struct{ confirmed, display int }

func (s *CellStore) reconcileVersions() {
	next := make(map[string]cellSourceVersion, len(s.items))
	for i := range s.items {
		cell := &s.items[i]
		key := itemIdentity(*cell, i)
		source := cellSourceVersion{kind: cell.kind, text: cell.text, status: cell.status,
			name: cell.toolName, args: cell.toolArgsRaw, truncated: cell.toolTruncated}
		old := s.versions[key]
		source.revision = old.revision
		if source != old || source.revision == 0 {
			s.sequence++
			source.revision = s.sequence
		}
		cell.revision = source.revision
		next[key] = source
	}
	s.versions = next
}
func (m *CellStore) compose() {
	// Reports are anchored at insertion time to the nearest confirmed message
	// (or to its ordinal when an event has no durable message id). Merge them
	// around the confirmed layer so a resync cannot move a /config result or an
	// error behind all later turns.
	buckets := make(map[int][]historyCell)
	for _, report := range m.reports {
		position := report.reportIndex
		if position < 0 {
			position = 0
		}
		if report.reportAnchor != "" {
			position = len(m.confirmedItems)
			for i, item := range m.confirmedItems {
				if item.messageID == report.reportAnchor {
					position = i + 1
					break
				}
			}
		}
		if position > len(m.confirmedItems) {
			position = len(m.confirmedItems)
		}
		buckets[position] = append(buckets[position], report)
	}
	m.items = make([]historyCell, 0, len(m.confirmedItems)+len(m.reports))
	for i := 0; i <= len(m.confirmedItems); i++ {
		m.items = append(m.items, buckets[i]...)
		if i < len(m.confirmedItems) {
			m.items = append(m.items, m.confirmedItems[i])
		}
	}
	m.positions = make(map[string]cellPosition, len(m.confirmedItems))
	for i, cell := range m.confirmedItems {
		if cell.messageID != "" {
			m.positions[cell.messageID] = cellPosition{confirmed: i, display: -1}
		}
	}
	for i, cell := range m.items {
		if pos, ok := m.positions[cell.messageID]; ok {
			pos.display = i
			m.positions[cell.messageID] = pos
		}
	}
	m.reconcileVersions()
}

// replace updates one stable cell without rebuilding the complete transcript.
// Snapshot/reorder operations still go through compose.
func (s *CellStore) replace(cell historyCell) bool {
	pos, ok := s.positions[cell.messageID]
	if !ok || pos.display < 0 || pos.confirmed >= len(s.confirmedItems) || pos.display >= len(s.items) {
		return false
	}
	if s.confirmedItems[pos.confirmed].messageID != cell.messageID || s.items[pos.display].messageID != cell.messageID {
		return false
	}
	source := cellSourceVersion{kind: cell.kind, text: cell.text, status: cell.status, name: cell.toolName, args: cell.toolArgsRaw, truncated: cell.toolTruncated}
	key := itemIdentity(cell, pos.display)
	old := s.versions[key]
	source.revision = old.revision
	if source != old || source.revision == 0 {
		s.sequence++
		source.revision = s.sequence
	}
	cell.revision = source.revision
	s.versions[key] = source
	s.confirmedItems[pos.confirmed] = cell
	s.items[pos.display] = cell
	return true
}
