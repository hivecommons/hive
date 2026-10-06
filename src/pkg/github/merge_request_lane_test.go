package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// laneRelayServer serves a green head "abc" for every PR and counts the
// relay's own merge and update-branch calls.
func laneRelayServer(t *testing.T, merges, updates *int) *httptest.Server {
	t.Helper()
	green := greenFixture()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if green.serveCI(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			*merges++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/update-branch"):
			*updates++
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"message":"Updating pull request branch."}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

type laneGateRecorder struct {
	reqs   []LaneMergeRequest
	result func(ctx context.Context, req LaneMergeRequest) (LaneMergeResult, error)
}

func (g *laneGateRecorder) gate(ctx context.Context, req LaneMergeRequest) (LaneMergeResult, error) {
	g.reqs = append(g.reqs, req)
	return g.result(ctx, req)
}

func serializedStrategy(string) string { return config.MergeStrategyHiveSerialized }

func runLaneRelay(t *testing.T, c *Client, req MergeRequest) string {
	t.Helper()
	dir := t.TempDir()
	mergeRequestDirForTest = dir
	t.Cleanup(func() { mergeRequestDirForTest = "" })
	reqPath, err := WriteMergeRequest(dir, req)
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())
	return reqPath
}

// AC2: a direct repo never reaches the lane gate and keeps today's calls,
// including the requested unpinned branch update and the relay's own merge.
func TestMergeRequestLane_DirectRepoTakesTodaysPath(t *testing.T) {
	merges, updates := 0, 0
	srv := laneRelayServer(t, &merges, &updates)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetSerializedLane(func(string) string { return config.MergeStrategyDirect }, func(context.Context, LaneMergeRequest) (LaneMergeResult, error) {
		t.Fatal("a direct repo must never consult the lane gate")
		return LaneMergeResult{}, nil
	})

	reqPath := runLaneRelay(t, c, MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner", UpdateBranch: true})
	if merges != 1 || updates != 1 {
		t.Fatalf("merges = %d, updates = %d; want today's update + merge", merges, updates)
	}
	if resp := readMergeResult(t, reqPath); !resp.OK || resp.SHA != "deadbeef" {
		t.Fatalf("result = %+v, want merged", resp)
	}
}

// AC7: a relay request for a PR that is not at the front of the lane is
// deferred with that reason: no merge call, no branch update, no attempt
// consumed, and the request stays queued for the next tick.
func TestMergeRequestLane_NotAtFrontIsDeferred(t *testing.T) {
	merges, updates := 0, 0
	srv := laneRelayServer(t, &merges, &updates)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	rec := &laneGateRecorder{result: func(context.Context, LaneMergeRequest) (LaneMergeResult, error) {
		return LaneMergeResult{Outcome: LaneOutcomeDeferred, Reason: "deferred: not at the front of the lane"}, nil
	}}
	c.SetSerializedLane(serializedStrategy, rec.gate)

	reqPath := runLaneRelay(t, c, MergeRequest{Repo: "r", Number: 42, ExpectSHA: "abc", Agent: "scanner", UpdateBranch: true})
	if merges != 0 || updates != 0 {
		t.Fatalf("merges = %d, updates = %d; a non-front PR gets neither", merges, updates)
	}
	if len(rec.reqs) != 1 || rec.reqs[0].Repo != "o/r" || rec.reqs[0].Number != 42 || rec.reqs[0].Path != PRAuditPathRelay {
		t.Fatalf("lane requests = %+v", rec.reqs)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("a deferred request must stay queued: %v", err)
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || resp.Attempts != 0 || !strings.Contains(resp.Error, "not at the front of the lane") {
		t.Fatalf("result = %+v, want a deferral that consumed no attempt", resp)
	}
}

// The front PR merges through the lane; the relay's authorization is
// re-checked for the exact head at the lane's final re-check, and the merge
// is audited and the request consumed as today.
func TestMergeRequestLane_FrontMergesWithRelayAuthorization(t *testing.T) {
	merges, updates := 0, 0
	srv := laneRelayServer(t, &merges, &updates)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	deny := false
	c.mergeAuthz = func(agent string, uid int, repo string, number int, expectSHA string) error {
		if deny {
			return errors.New("no longer merge-eligible")
		}
		return nil
	}
	rec := &laneGateRecorder{result: func(ctx context.Context, req LaneMergeRequest) (LaneMergeResult, error) {
		if err := req.Authorize(ctx, "other"); err == nil {
			t.Error("authorization must be bound to the requested expect_sha")
		}
		deny = true
		if err := req.Authorize(ctx, "abc"); err == nil {
			t.Error("authorization must re-run the relay's authorizer")
		}
		deny = false
		if err := req.Authorize(ctx, "abc"); err != nil {
			t.Errorf("authorization for the requested head: %v", err)
		}
		return LaneMergeResult{Outcome: LaneOutcomeMerged, Reason: "merged", SHA: "lanesha", Method: "squash"}, nil
	}}
	c.SetSerializedLane(serializedStrategy, rec.gate)

	reqPath := runLaneRelay(t, c, MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	if merges != 0 {
		t.Fatalf("the relay itself made %d merge calls; only the lane merges", merges)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Fatal("a merged request must be consumed")
	}
	if resp := readMergeResult(t, reqPath); !resp.OK || resp.SHA != "lanesha" {
		t.Fatalf("result = %+v, want merged through the lane", resp)
	}
}

// AC26 and fail-closed outcomes: a native merge-queue branch, a PR that
// left the front, a lane error or a missing gate are failed attempts with
// the lane's reason; nothing is merged.
func TestMergeRequestLane_RefusalsAreFailedAttempts(t *testing.T) {
	for _, tt := range []struct {
		name string
		gate SerializedLaneGate
		want string
	}{
		{name: "native merge queue", gate: func(context.Context, LaneMergeRequest) (LaneMergeResult, error) {
			return LaneMergeResult{Outcome: LaneOutcomeRefused, Reason: `target branch "main" has GitHub's native merge queue`}, nil
		}, want: "native merge queue"},
		{name: "left the front", gate: func(context.Context, LaneMergeRequest) (LaneMergeResult, error) {
			return LaneMergeResult{Outcome: LaneOutcomeLeft, Reason: "pull request has merge conflicts"}, nil
		}, want: "merge conflicts"},
		{name: "lane error", gate: func(context.Context, LaneMergeRequest) (LaneMergeResult, error) {
			return LaneMergeResult{}, errors.New("lane record unwritable")
		}, want: "lane record unwritable"},
		{name: "no gate", want: ReasonLaneUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			merges, updates := 0, 0
			srv := laneRelayServer(t, &merges, &updates)
			defer srv.Close()
			c := testMergeClient(t, srv.URL)
			c.SetSerializedLane(serializedStrategy, tt.gate)

			reqPath := runLaneRelay(t, c, MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
			if merges != 0 {
				t.Fatalf("merges = %d, want none", merges)
			}
			resp := readMergeResult(t, reqPath)
			if resp.OK || resp.Attempts != 1 || !strings.Contains(resp.Error, tt.want) {
				t.Fatalf("result = %+v, want a failed attempt naming %q", resp, tt.want)
			}
		})
	}
}

// A waiting or updated front PR keeps its request queued without an attempt.
func TestMergeRequestLane_WaitingFrontStaysQueued(t *testing.T) {
	merges, updates := 0, 0
	srv := laneRelayServer(t, &merges, &updates)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetSerializedLane(serializedStrategy, func(context.Context, LaneMergeRequest) (LaneMergeResult, error) {
		return LaneMergeResult{Outcome: LaneOutcomeUpdated, Reason: "merged the tip in"}, nil
	})
	reqPath := runLaneRelay(t, c, MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	resp := readMergeResult(t, reqPath)
	if merges != 0 || resp.OK || resp.Attempts != 0 || resp.Error != "merged the tip in" {
		t.Fatalf("merges = %d, result = %+v", merges, resp)
	}
}

// AC4: the strategy never turns merging on. A paused repo or a denied
// authorization stops the relay before the lane is consulted.
func TestMergeRequestLane_PauseAndAuthorizationComeFirst(t *testing.T) {
	merges, updates := 0, 0
	srv := laneRelayServer(t, &merges, &updates)
	defer srv.Close()
	never := func(context.Context, LaneMergeRequest) (LaneMergeResult, error) {
		t.Fatal("the lane must not be consulted")
		return LaneMergeResult{}, nil
	}

	paused := testMergeClient(t, srv.URL)
	paused.SetSerializedLane(serializedStrategy, never)
	paused.SetRepoPausedFunc(func(string) bool { return true })
	reqPath := runLaneRelay(t, paused, MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Fatalf("paused repo request should be denied: %v", err)
	}

	denied := testMergeClient(t, srv.URL)
	denied.SetSerializedLane(serializedStrategy, never)
	denied.mergeAuthz = func(string, int, string, int, string) error { return errors.New("below Level 6") }
	reqPath = runLaneRelay(t, denied, MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Fatalf("unauthorized request should be denied: %v", err)
	}
	if merges != 0 || updates != 0 {
		t.Fatalf("merges = %d, updates = %d; want none", merges, updates)
	}
}

// A request without expect_sha pins nothing, as today; the lane's final
// re-check still re-runs the relay's authorizer for the head it merges.
func TestMergeRequestLane_UnpinnedRequestAuthorizesLaneHead(t *testing.T) {
	merges, updates := 0, 0
	srv := laneRelayServer(t, &merges, &updates)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	var authorized []string
	c.mergeAuthz = func(agent string, uid int, repo string, number int, expectSHA string) error {
		authorized = append(authorized, expectSHA)
		return nil
	}
	c.SetSerializedLane(serializedStrategy, func(ctx context.Context, req LaneMergeRequest) (LaneMergeResult, error) {
		if err := req.Authorize(ctx, "lanehead"); err != nil {
			t.Errorf("unpinned request at the lane head: %v", err)
		}
		return LaneMergeResult{Outcome: LaneOutcomeMerged, SHA: "lanesha"}, nil
	})
	reqPath := runLaneRelay(t, c, MergeRequest{Repo: "o/r", Number: 42, Agent: "scanner"})
	if resp := readMergeResult(t, reqPath); !resp.OK || len(authorized) != 2 || authorized[1] != "lanehead" {
		t.Fatalf("result = %+v, authorized = %v", resp, authorized)
	}
}

func TestSerializedLaneAccessors(t *testing.T) {
	var nilClient *Client
	nilClient.SetSerializedLane(serializedStrategy, nil)
	if s, g := nilClient.SerializedLane(); s != nil || g != nil {
		t.Fatal("nil client has no lane")
	}
	if SerializedStrategy(nil, "r") {
		t.Fatal("no strategy resolver means direct")
	}
	if !SerializedStrategy(serializedStrategy, "r") || SerializedStrategy(func(string) string { return "" }, "r") {
		t.Fatal("only hive-serialized routes through the lane")
	}
	if res, err := RunSerializedLane(context.Background(), nil, LaneMergeRequest{}); err != nil || res.Outcome != LaneOutcomeRefused || res.Merged() {
		t.Fatalf("RunSerializedLane(nil) = %+v, %v; want refused", res, err)
	}
}
