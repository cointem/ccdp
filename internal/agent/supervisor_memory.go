package agent

import "ccdp/internal/session"

// In-process continuation preserves the same log and artifacts even when
// persistence is disabled. It never creates a hidden on-disk session.
func (p *sessionPersistence) restoreMemoryRun(records []session.Record, blobs *memoryRequestArtifacts) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	store := session.NewMemoryStore()
	for i := 0; i < len(records); {
		batch := session.Batch{TransactionID: records[i].TransactionID}
		for i < len(records) && records[i].TransactionID == batch.TransactionID {
			batch.Events = append(batch.Events, records[i].Event)
			i++
		}
		if _, err := store.Commit(store.CurrentCursor(), batch); err != nil {
			_ = store.Close()
			return err
		}
	}
	_ = p.store.Close()
	p.store = store
	p.memoryArtifacts = blobs
	p.projection = nil
	p.projectionCursor = 0
	p.transcript = transcriptFromRecords(records)
	return nil
}
