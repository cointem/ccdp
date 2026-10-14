package agent

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"

	"ccdp/internal/atomicfile"
	"ccdp/internal/session"
)

type reviewSaveError struct {
	cause error
	id    string
}

func (e *reviewSaveError) Error() string {
	return fmt.Sprintf("report save failed: %v; retry with /review save %s", e.cause, e.id)
}

func (e *reviewSaveError) Unwrap() error { return e.cause }

type pendingReview struct {
	row  session.ReviewRecorded
	text string
}

// Saving retries only the ordinary file and its session record, never the model.
func (a *Agent) saveReview(id string) error {
	state := a.codingState()
	state.reviewMu.Lock()
	defer state.reviewMu.Unlock()
	report, ok := state.pendingReviews[id]
	if !ok {
		return errors.New("no pending report with this id")
	}
	a.mu.Lock()
	cfg, sid := *a.services.cfg, a.conversation.sessionID
	a.mu.Unlock()
	row := report.row
	if cfg.NoSessionPersistence {
		row.ReportText = report.text
	} else {
		dir := filepath.Join(cfg.SessionDir, sid, "reviews")
		row.ReportPath = base64.RawURLEncoding.EncodeToString([]byte(id)) + ".md"
		if err := atomicfile.WriteFile(filepath.Join(dir, row.ReportPath), []byte(report.text), 0600); err != nil {
			return err
		}
	}
	if _, err := a.commitAuxiliary("review-"+id, row); err != nil {
		return err
	}
	delete(state.pendingReviews, id)
	if a.services.supervisor != nil {
		var runs []*managedRun
		for _, r := range a.services.supervisor.directory.Children() {
			runs = append(runs, r)
		}
		for _, r := range runs {
			r.mu.Lock()
			if r.parent == a && r.reviewCommand != nil && string(r.reviewCommand.ID) == id && !r.fact.Child.Run.Active() && r.fact.Child.Run.SaveError != "" {
				r.fact.Child.Run.Error = ""
				r.fact.Child.Run.SaveError = ""
				switch row.Status {
				case "success":
					r.fact.Child.Run.Status = "succeeded"
				case "partial":
					r.fact.Child.Run.Status = "partial"
				case "cancelled":
					r.fact.Child.Run.Status = "cancelled"
				default:
					r.fact.Child.Run.Status = "failed"
					r.fact.Child.Run.Error = "审查失败；报告已保存，详见正文。"
				}
				r.err = nil
				if err := a.services.supervisor.persist(r, "report-saved"); err != nil {
					r.mu.Unlock()
					return err
				}
			}
			r.mu.Unlock()
		}
	}
	return nil
}
