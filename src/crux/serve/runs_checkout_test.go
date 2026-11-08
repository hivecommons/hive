package serve

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

// withRuns adds a contained-run service wired to the fixture's checkout
// service both ways, as crux serve does: runs resolve checkout ids, and
// the checkout service asks the run service what is in use. The launcher
// holds every command until release is closed.
func (f *checkoutFixture) withRuns(release <-chan struct{}) {
	f.t.Helper()
	store, err := runstate.Open(f.t.TempDir())
	if err != nil {
		f.t.Fatal(err)
	}
	launcher := runsLauncher(func(ctx context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		select {
		case <-release:
			return sandbox.Result{ExitCode: 0, Stdout: "ok"}, nil
		case <-ctx.Done():
			return sandbox.Result{ExitCode: -1}, ctx.Err()
		}
	})
	backend := &runs.Backend{Name: "fake", Launcher: launcher, Capabilities: runs.Capabilities{NetworkNone: true}}
	svc := runs.New(store, backend, runs.Config{Image: "img"})
	svc.Checkouts = f.s.Checkouts
	f.s.Runs = svc
	f.h = f.s.Handler()
}

func TestRunsInACheckoutAreOwnerGatedAndHoldTheCheckout(t *testing.T) {
	repo, sha := checkoutFixtureRepo(t)
	f := newCheckoutFixture(t, repo)
	release := make(chan struct{})
	f.withRuns(release)

	var mine api.Checkout
	if code, e := f.call("atlas", http.MethodPost, api.PathCheckouts, api.CheckoutRequest{Repo: repo, Commit: sha}, &mine); code != http.StatusCreated {
		t.Fatalf("atlas create checkout: %d %+v", code, e)
	}
	start := func(who, id string) (int, api.ContainedRun, api.Error) {
		var run api.ContainedRun
		code, e := f.call(who, http.MethodPost, api.PathRuns, api.ContainedRunRequest{Checkout: id, Command: []string{"make"}, Detach: true}, &run)
		return code, run, e
	}

	// Refusals never reach the launcher: another project's checkout is
	// forbidden, an unknown id is a bad request.
	for _, tc := range []struct {
		name, who, id string
		status        int
		code, msg     string
	}{
		{"another project's checkout", "beacon", mine.ID, http.StatusForbidden, api.CodeForbidden, `owned by "atlas", not by "beacon"`},
		{"unknown checkout", "atlas", "chk_000000000000", http.StatusBadRequest, api.CodeBadRequest, "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, e := start(tc.who, tc.id); code != tc.status || e.Code != tc.code || !strings.Contains(e.Error, tc.msg) {
				t.Fatalf("start: %d %+v, want %d %s containing %q", code, e, tc.status, tc.code, tc.msg)
			}
		})
	}

	// The owner runs in it; the record names the checkout and its path.
	code, run, e := start("atlas", mine.ID)
	if code != http.StatusAccepted || run.Owner != "atlas" || run.Checkout != mine.ID || run.Status != api.RunAccepted {
		t.Fatalf("atlas start: %d %+v %+v", code, run, e)
	}
	// Another project can neither read nor cancel it; the operator can read it.
	if code, e := f.call("beacon", http.MethodGet, api.RunPath(run.ID), nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon get atlas's run: %d %+v", code, e)
	}
	if code, e := f.call("beacon", http.MethodPost, api.RunCancelPath(run.ID), nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon cancel atlas's run: %d %+v", code, e)
	}
	if code, e := f.call("beacon", http.MethodGet, api.RunCmdlogPath(run.ID), nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon read atlas's command log: %d %+v", code, e)
	}
	var seen api.ContainedRun
	if code, _ := f.call("operator", http.MethodGet, api.RunPath(run.ID), nil, &seen); code != http.StatusOK || seen.ID != run.ID {
		t.Fatalf("operator get: %d %+v", code, seen)
	}
	// While the run is live the checkout cannot be deleted, even by its owner.
	if code, e := f.call("atlas", http.MethodDelete, api.PathCheckouts+"/"+mine.ID, nil, nil); code != http.StatusConflict || e.Code != api.CodeConflict {
		t.Fatalf("delete checkout in use: %d %+v", code, e)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done, err := f.s.Runs.Wait(ctx, run.ID)
	if err != nil || done.Status != api.RunCompleted || done.ExitCode == nil || *done.ExitCode != 0 || done.Workdir != mine.Path || done.Checkout != mine.ID {
		t.Fatalf("finished run = %+v err=%v (checkout path %s)", done, err, mine.Path)
	}
	if code, _ := f.call("atlas", http.MethodGet, api.RunPath(run.ID), nil, &seen); code != http.StatusOK || seen.Stdout != "ok" {
		t.Fatalf("atlas get finished: %d %+v", code, seen)
	}
	// Settled runs release the checkout.
	if code, e := f.call("atlas", http.MethodDelete, api.PathCheckouts+"/"+mine.ID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete after the run settled: %d %+v", code, e)
	}
	// Agent RPCs still refuse the project token even though runs admit it.
	if code, e := f.call("atlas", http.MethodGet, api.PathHealth, nil, nil); code != http.StatusUnauthorized || e.Code != api.CodeUnauthorized {
		t.Fatalf("project token on health: %d %+v", code, e)
	}
}
