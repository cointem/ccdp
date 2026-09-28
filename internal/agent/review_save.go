package agent

import (
	"ccdp/internal/session"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	cfg, sid := *a.cfg, a.sessionID
	a.mu.Unlock()
	row := report.row
	if cfg.NoSessionPersistence {
		row.ReportText = report.text
	} else {
		dir := filepath.Join(cfg.SessionDir, sid, "reviews")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		f, err := os.CreateTemp(dir, ".report-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		_, err = f.WriteString(report.text)
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
		row.ReportPath = base64.RawURLEncoding.EncodeToString([]byte(id)) + ".md"
		if err = os.Rename(f.Name(), filepath.Join(dir, row.ReportPath)); err != nil {
			return err
		}
	}
	if _, err := a.persistenceHandle().commitEvents("review-"+id, row); err != nil {
		return err
	}
	delete(state.pendingReviews, id)
	if a.supervisor != nil {
		a.supervisor.mu.Lock()
		var runs []*managedRun
		for _, r := range a.supervisor.children {
			runs = append(runs, r)
		}
		a.supervisor.mu.Unlock()
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
				if err := a.supervisor.persist(r, "report-saved"); err != nil {
					r.mu.Unlock()
					return err
				}
			}
			r.mu.Unlock()
		}
	}
	return nil
}
