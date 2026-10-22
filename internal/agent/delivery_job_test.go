package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccdp/internal/changes"
	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

func TestDeliveryRecordFailureRetainsCandidateUntilCommitIsDurable(t *testing.T) {
	for _, stage := range []string{"checked", "committing"} {
		t.Run(stage, func(t *testing.T) {
			candidate, err := os.MkdirTemp("", "ccdp-delivery-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(candidate) })
			root := t.TempDir()
			store := changes.New("")
			original, err := store.PutValue([]byte{})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := store.SaveSnapshot(changes.Snapshot{Root: root, Files: map[string]changes.File{}})
			if err != nil {
				t.Fatal(err)
			}
			head, commit, tree := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
			durable := session.DeliveryRecorded{PlanID: "plan", Stage: stage, Candidate: candidate, Tree: tree}
			recordFailure := errors.New("candidate commit record failed")
			publishFailure := errors.New("publication paused after durable candidate commit")
			failRecord := true
			commits := 0
			j := deliveryJob{
				id: "plan", root: root, policy: sandbox.New(root), store: store, state: durable,
				plan: deliveryPlan{Root: root, Head: head, IndexPath: filepath.Join(root, "index"),
					OriginalIndex: original, Selected: selected.ID, CreatedAt: time.Now()},
				git: func(_ context.Context, args ...string) ([]byte, error) {
					if args[0] == "rev-parse" {
						return []byte(".git/hooks"), nil
					}
					return nil, errors.New("no hooks configuration")
				},
				record: func(s session.DeliveryRecorded) error {
					if s.Stage == "candidate_committed" && failRecord {
						return recordFailure
					}
					durable = s
					return nil
				},
			}
			j.head = func(context.Context) (string, error) {
				if j.state.Stage == "candidate_committed" {
					return "", publishFailure
				}
				return head, nil
			}
			j.exec = func(_ context.Context, _ string, _ *sandbox.Sandbox, argv []string, _ io.Reader, _ []string) (execution.Result, error) {
				command := strings.Join(argv, " ")
				switch command {
				case "git rev-parse HEAD":
					return execution.Result{Stdout: commit}, nil
				case "git write-tree", "git rev-parse HEAD^{tree}":
					return execution.Result{Stdout: tree}, nil
				case "git rev-list --parents -n 1 " + commit:
					return execution.Result{Stdout: commit + " " + head}, nil
				case "git diff --exit-code":
					return execution.Result{}, nil
				default:
					if strings.Contains(command, " commit ") {
						commits++
						return execution.Result{}, nil
					}
					if strings.Contains(command, " fetch ") {
						return execution.Result{}, nil
					}
					t.Fatalf("unexpected command: %q", argv)
					return execution.Result{}, nil
				}
			}
			if _, err := j.run(context.Background()); !errors.Is(err, recordFailure) {
				t.Fatalf("record failure: %v", err)
			}
			if durable.Stage != "committing" || durable.Commit != "" {
				t.Fatalf("failed record unexpectedly persisted: %+v", durable)
			}
			if _, err := os.Stat(candidate); err != nil {
				t.Fatalf("recovery candidate removed before durable commit: %v", err)
			}
			// Restart from the actual saved state. The candidate is queried and
			// imported without running git commit again.
			j.state, failRecord = durable, false
			before := commits
			if _, err := j.run(context.Background()); !errors.Is(err, publishFailure) {
				t.Fatalf("resume failed before publication: %v", err)
			}
			if commits != before || durable.Stage != "candidate_committed" || durable.Commit != commit {
				t.Fatalf("resume repeated commit or lost recovered identity: commits=%d, %+v", commits, durable)
			}
			if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("durable candidate was not cleaned up: %v", err)
			}
		})
	}
}

func TestDeliveryPushRecoveryQueriesBeforeRetry(t *testing.T) {
	for _, remoteHasCommit := range []bool{true, false} {
		t.Run(map[bool]string{true: "already-pushed", false: "not-pushed"}[remoteHasCommit], func(t *testing.T) {
			var effects []string
			j := deliveryJob{
				plan:   deliveryPlan{Mode: "push", Remote: "origin", RemoteURL: "https://example.invalid/repo.git", Ref: "refs/heads/main"},
				state:  session.DeliveryRecorded{Stage: "push_requested", Commit: strings.Repeat("a", 40)},
				policy: &sandbox.Sandbox{AllowNetwork: true},
				git: func(context.Context, ...string) ([]byte, error) {
					return []byte("https://example.invalid/repo.git"), nil
				},
				record: func(s session.DeliveryRecorded) error { effects = append(effects, "record:"+s.Stage); return nil },
			}
			j.exec = func(_ context.Context, _ string, _ *sandbox.Sandbox, argv []string, _ io.Reader, _ []string) (execution.Result, error) {
				effects = append(effects, argv[1])
				switch argv[1] {
				case "ls-remote":
					if remoteHasCommit {
						return execution.Result{Stdout: j.state.Commit + "\t" + j.plan.Ref + "\n"}, nil
					}
					return execution.Result{}, nil
				case "push":
					return execution.Result{}, nil
				}
				return execution.Result{}, errors.New("unexpected command")
			}
			if _, err := j.publishRemote(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := "ls-remote,record:pushed,record:completed"
			if !remoteHasCommit {
				want = "ls-remote,record:push_requested,push,record:pushed,record:completed"
			}
			if strings.Join(effects, ",") != want {
				t.Fatal(effects)
			}
		})
	}
}
