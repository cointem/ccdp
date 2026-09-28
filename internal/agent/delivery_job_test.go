package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"ccdp/internal/execution"
	"ccdp/internal/sandbox"
	"ccdp/internal/session"
)

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
