package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ccdp/internal/atomicfile"
)

type quarantineRecord struct {
	Roots      []string  `json:"roots"`
	Reason     string    `json:"reason"`
	RecordedAt time.Time `json:"recorded_at"`
}

// CheckExecution preserves unconfirmed cleanup across session restarts. It
// restricts overlapping writable resources, not unrelated network access.
func (s *Sandbox) CheckExecution() error {
	if s == nil {
		return errors.New("execution environment unavailable")
	}
	policy := s.Snapshot()
	if policy.QuarantineStore == "" {
		return nil
	}
	b, err := os.ReadFile(policy.QuarantineStore)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read execution quarantine: %w", err)
	}
	var records []quarantineRecord
	if err := json.Unmarshal(b, &records); err != nil {
		return fmt.Errorf("invalid execution quarantine: %w", err)
	}
	roots := append([]string{policy.Workspace}, policy.AdditionalDirs...)
	for _, record := range records {
		for _, blocked := range record.Roots {
			for _, root := range roots {
				if blocked != "" && root != "" && (pathWithin(canonicalRoot(blocked), canonicalRoot(root)) || pathWithin(canonicalRoot(root), canonicalRoot(blocked))) {
					return fmt.Errorf("execution environment quarantined: %s; cleanup must be independently confirmed (record: %s)", record.Reason, policy.QuarantineStore)
				}
			}
		}
	}
	return nil
}

func (s *Sandbox) RecordQuarantine(reason string) error {
	policy := s.Snapshot()
	if policy.QuarantineStore == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(policy.QuarantineStore), 0700); err != nil {
		return err
	}
	lock, err := atomicfile.AcquireLock(policy.QuarantineStore + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	var records []quarantineRecord
	b, err := os.ReadFile(policy.QuarantineStore)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &records); err != nil {
			return err
		}
	}
	roots := policy.ExecutionRoots()
	merged := false
	for i := range records {
		if rootCovered(policy.Workspace, records[i].Roots) {
			for _, root := range roots {
				if !rootCovered(root, records[i].Roots) {
					records[i].Roots = append(records[i].Roots, root)
				}
			}
			merged = true
			break
		}
	}
	if !merged && len(records) >= 1024 {
		return fmt.Errorf("execution quarantine record limit reached")
	}
	if !merged {
		records = append(records, quarantineRecord{Roots: roots, Reason: reason, RecordedAt: time.Now().UTC()})
	}
	b, err = json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(policy.QuarantineStore, b, 0600)
}
