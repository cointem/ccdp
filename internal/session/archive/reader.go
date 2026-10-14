package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ccdp/internal/session"
)

func Load(dir, id string) (*Snapshot, error) {
	if dir == "" || id == "" {
		return nil, fmt.Errorf("agent: session dir and id are required")
	}
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	store, err := session.OpenJSONLReadOnly(dir, id)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	records, err := store.Read(session.Beginning)
	if err != nil {
		return nil, err
	}
	snapshot, err := FromRecords(records, id)
	if err == nil {
		snapshot.Records = records
		snapshot.Artifacts = session.OpenArtifactStoreReadOnly(filepath.Join(dir, id))
	}
	return snapshot, err
}

// List returns authoritative JSONL sessions, newest first, plus an
// issue for every damaged session it had to skip. It never writes or repairs.
func List(dir string) ([]Snapshot, []Issue, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var sessions []Snapshot
	var issues []Issue
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		if ValidateID(id) != nil {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(dir, id, "events.v1.jsonl")); statErr != nil {
			continue
		}
		snap, err := Load(dir, id)
		if err != nil {
			issues = append(issues, Issue{ID: id, Reason: err.Error()})
			continue
		}
		if info, infoErr := os.Stat(filepath.Join(dir, id, "events.v1.jsonl")); infoErr == nil && info.ModTime().After(snap.UpdatedAt) {
			snap.UpdatedAt = info.ModTime()
		}
		sessions = append(sessions, *snap)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, issues, nil
}

type Issue struct {
	ID     string
	Reason string
}

func ValidateID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("agent: invalid session id %q", id)
	}
	return nil
}
