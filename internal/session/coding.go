package session

import (
	"errors"
	"time"
)

// RecoveryPointRecorded binds a workspace snapshot to the pre-input context.
// A later record for the same ID seals its post-turn snapshot.
type RecoveryPointRecorded struct {
	ID        string    `json:"id"`
	TurnID    string    `json:"turn_id"`
	Workspace string    `json:"workspace"`
	Before    string    `json:"before"`
	After     string    `json:"after,omitempty"`
	History   string    `json:"history"`
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

const EventTypeRecoveryPointRecorded EventType = "RecoveryPointRecorded"

func (RecoveryPointRecorded) Type() EventType { return EventTypeRecoveryPointRecorded }
func (RecoveryPointRecorded) seal()           {}
func (e RecoveryPointRecorded) validate() error {
	if e.ID == "" || e.Workspace == "" || len(e.Before) != 64 || len(e.History) != 64 {
		return errors.New("invalid recovery point")
	}
	return nil
}

type ReviewRecorded struct {
	Scope      string    `json:"scope,omitempty"`
	BaseOID    string    `json:"base_oid,omitempty"`
	TargetOID  string    `json:"target_oid,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	ReportPath string    `json:"report_path,omitempty"`
	ReportText string    `json:"report_text,omitempty"`
	ID         string    `json:"id"`
	SnapshotID string    `json:"snapshot_id"`
	ReportHash string    `json:"report_hash"`
	Status     string    `json:"status"`
}

const EventTypeReviewRecorded EventType = "ReviewRecorded"

func (ReviewRecorded) Type() EventType { return EventTypeReviewRecorded }
func (ReviewRecorded) seal()           {}
func (e ReviewRecorded) validate() error {
	if e.ID == "" || (e.ReportPath == "" && e.ReportText == "" && (len(e.SnapshotID) != 64 || len(e.ReportHash) != 64)) {
		return errors.New("invalid review record")
	}
	return nil
}

type FileMutationRecorded struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	Path      string    `json:"path"`
	Before    string    `json:"before"`
	After     string    `json:"after,omitempty"`
	TurnID    string    `json:"turn_id"`
	CreatedAt time.Time `json:"created_at"`
}

const EventTypeFileMutationRecorded EventType = "FileMutationRecorded"

func (FileMutationRecorded) Type() EventType { return EventTypeFileMutationRecorded }
func (FileMutationRecorded) seal()           {}
func (e FileMutationRecorded) validate() error {
	if e.ID == "" || e.Path == "" || e.Workspace == "" || len(e.Before) != 64 {
		return errors.New("invalid mutation")
	}
	return nil
}

// RestoreRecorded keeps code and context recovery idempotent across restarts.
type RestoreRecorded struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	History string `json:"history"`
	Backup  string `json:"backup"`
}

const EventTypeRestoreRecorded EventType = "RestoreRecorded"

func (RestoreRecorded) Type() EventType { return EventTypeRestoreRecorded }
func (RestoreRecorded) seal()           {}
func (e RestoreRecorded) validate() error {
	if e.ID == "" || (e.History != "" && len(e.History) != 64) || e.Backup == "" || (e.Status != "started" && e.Status != "completed") {
		return errors.New("invalid restore record")
	}
	return nil
}
