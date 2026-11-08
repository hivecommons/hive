package serve

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/lanes"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

// newCmdlogFixture is a token-bearing server with a run service whose
// launcher echoes the command, and two lane projects (atlas, beacon)
// besides the operator. It returns the workspace runs may use.
func newCmdlogFixture(t *testing.T) (*checkoutFixture, string) {
	t.Helper()
	tokens := map[string]string{"atlas": strings.Repeat("a", 32), "beacon": strings.Repeat("b", 32), "operator": "agent-admin"}
	cfg := lanes.Config{Principals: []lanes.Principal{{Project: "atlas", TokenEnv: "ATLAS"}, {Project: "beacon", TokenEnv: "BEACON"}}}
	auth, err := lanes.Bind(cfg, func(env string) string { return tokens[strings.ToLower(env)] }, tokens["operator"])
	if err != nil {
		t.Fatal(err)
	}
	store, err := runstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ws := filepath.Join(root, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := runsLauncher(func(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: strings.Join(spec.Command, " ") + "\n"}, nil
	})
	backend := &runs.Backend{Name: "fake", Launcher: launcher, Capabilities: runs.Capabilities{NetworkNone: true}}
	f := &checkoutFixture{t: t, tokens: tokens, s: New(newFakeRuntime())}
	f.s.Token, f.s.LaneAuth = tokens["operator"], auth
	f.s.Runs = runs.New(store, backend, runs.Config{Image: "img", WorkspaceRoots: []string{root}})
	f.h = f.s.Handler()
	return f, ws
}

func TestRunCmdlogIsOwnerOrOperatorAndPaged(t *testing.T) {
	f, ws := newCmdlogFixture(t)
	var run api.ContainedRun
	if code, e := f.call("atlas", http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"make", "test"}}, &run); code != http.StatusOK || run.Status != api.RunCompleted {
		t.Fatalf("atlas run: %d %+v %+v", code, run, e)
	}

	var page api.CommandLog
	if code, e := f.call("atlas", http.MethodGet, api.RunCmdlogPath(run.ID), nil, &page); code != http.StatusOK {
		t.Fatalf("owner read: %d %+v", code, e)
	}
	if page.Run != run.ID || len(page.Commands) != 1 || page.NextSeq != 2 || page.More {
		t.Fatalf("page = %+v", page)
	}
	rec := page.Commands[0]
	if strings.Join(rec.Command, " ") != "make test" || rec.Cwd != ws || rec.Status != api.CommandCompleted || rec.Capture != api.CaptureRunner ||
		rec.ExitCode == nil || *rec.ExitCode != 0 || rec.Stdout == nil || rec.Stdout.Head != "make test\n" {
		t.Fatalf("record = %+v", rec)
	}

	// The operator reads it too; another project cannot.
	if code, e := f.call("operator", http.MethodGet, api.RunCmdlogPagePath(run.ID, 0, 5), nil, &page); code != http.StatusOK || len(page.Commands) != 1 {
		t.Fatalf("operator read: %d %+v %+v", code, page, e)
	}
	if code, e := f.call("beacon", http.MethodGet, api.RunCmdlogPath(run.ID), nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon read: %d %+v", code, e)
	}

	// Paging past the end is an empty page that keeps the cursor; a huge
	// limit is capped rather than refused.
	if code, _ := f.call("atlas", http.MethodGet, api.RunCmdlogPagePath(run.ID, 2, 1<<20), nil, &page); code != http.StatusOK || len(page.Commands) != 0 || page.NextSeq != 2 {
		t.Fatalf("caught-up page: %d %+v", code, page)
	}

	for _, q := range []string{"?since_seq=-1", "?since_seq=x", "?limit=-2", "?limit=1.5"} {
		if code, e := f.call("atlas", http.MethodGet, api.RunCmdlogPath(run.ID)+q, nil, nil); code != http.StatusBadRequest || e.Code != api.CodeBadRequest {
			t.Fatalf("query %s: %d %+v", q, code, e)
		}
	}
	if code, e := f.call("atlas", http.MethodGet, api.RunCmdlogPath("run_missing"), nil, nil); code != http.StatusNotFound || e.Code != api.CodeNotFound {
		t.Fatalf("unknown run: %d %+v", code, e)
	}

	// A log that cannot be folded is a server failure, never a partial
	// history.
	if err := os.WriteFile(cmdlog.Path(f.s.Runs.Store.Path(run.ID)), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, e := f.call("atlas", http.MethodGet, api.RunCmdlogPath(run.ID), nil, nil); code != http.StatusInternalServerError || e.Code != api.CodeRuntime {
		t.Fatalf("corrupt log: %d %+v", code, e)
	}
}

func TestRunCmdlogNotSupportedWithoutRuns(t *testing.T) {
	f := newFamServer(t)
	f.expectCode(http.MethodGet, api.RunCmdlogPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
}

func TestRunCmdlogSeesALiveCommand(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	f, ws := newRunsServer(t, func(ctx context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		started <- struct{}{}
		select {
		case <-release:
			return sandbox.Result{}, nil
		case <-ctx.Done():
			return sandbox.Result{ExitCode: -1}, ctx.Err()
		}
	})
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"sleep"}, Detach: true}, http.StatusAccepted, &run)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("launcher never started")
	}
	// The launcher runs after the started line is written.
	var page api.CommandLog
	f.expect(http.MethodGet, api.RunCmdlogPath(run.ID), nil, http.StatusOK, &page)
	if len(page.Commands) != 1 || page.Commands[0].Status != api.CommandUnfinished || page.Commands[0].EndedAt != nil {
		t.Fatalf("live = %+v", page.Commands[0])
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.srv.Runs.Wait(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	f.expect(http.MethodGet, api.RunCmdlogPagePath(run.ID, page.NextSeq, 0), nil, http.StatusOK, &page)
	if len(page.Commands) != 1 || page.Commands[0].Status != api.CommandCompleted {
		t.Fatalf("after the end = %+v", page)
	}
}
