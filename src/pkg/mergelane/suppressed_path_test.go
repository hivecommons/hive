package mergelane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// writeCheckGH adds the PathWriteChecker capability to the fake: suppressed
// PRs get writeback containment's refusal, err (when set) is an unknown
// answer, and checks records every PR asked about.
type writeCheckGH struct {
	*fakeGH
	suppressed map[int]bool
	err        error
	checks     []int
}

func (g *writeCheckGH) PathCanWrite(_ context.Context, _ string, n int) error {
	g.checks = append(g.checks, n)
	if g.err != nil {
		return g.err
	}
	if g.suppressed[n] {
		return fmt.Errorf("%w: #%d carries none of hive-review; contributor_adopt needs the label at L6", writeback.ErrSuppressed, n)
	}
	return nil
}

func newWriteCheckHarness(t *testing.T) (*harness, *writeCheckGH) {
	t.Helper()
	h := newGateHarness(t)
	g := &writeCheckGH{fakeGH: h.f, suppressed: map[int]bool{}}
	store, err := OpenStore(h.path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	opts := h.options(store)
	opts.GitHub = g
	if h.lane, err = New(opts); err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, g
}

func (h *harness) gateVia(pr int, path string) ghub.LaneMergeResult {
	h.t.Helper()
	res, err := h.lane.Merge(context.Background(), ghub.LaneMergeRequest{
		Repo: testRepo, Branch: testBranch, Number: pr, Path: path, Authorize: h.authorize,
	})
	if err != nil {
		h.t.Fatalf("Merge(#%d via %s): %v", pr, path, err)
	}
	return res
}

// beyondRules drops the native-merge-queue rules read every gate call already
// makes before the lane runs.
func beyondRules(calls []string) []string {
	var out []string
	for _, c := range calls {
		if c != "RequiredChecks" {
			out = append(out, c)
		}
	}
	return out
}

const suppressedReason = "the sweep merge path cannot update or merge this pull request, so it takes no lane place"

// The live case: green auto-promoted contributor PRs the sweep would merge
// but whose branch update and merge writeback containment refuses (no
// hive-review label at L6). They used to take the front one at a time and
// hold it for the full front timeout while an owner-queued PR waited. Now
// they take no lane place, sweep after sweep, with no front churn, and the
// queued PR reaches the front on its first call and merges.
func TestSuppressedSweepPRTakesNoLanePlace(t *testing.T) {
	h, g := newWriteCheckHarness(t)
	for _, n := range []int{1, 2, 3} {
		h.f.addPR(n, fmt.Sprintf("h%d", n))
	}
	g.suppressed[1], g.suppressed[2] = true, true

	for sweep := 0; sweep < 3; sweep++ {
		for _, n := range []int{1, 2} {
			expectGate(t, h.gateVia(n, ghub.PRAuditPathSweep), ghub.LaneOutcomeRefused, suppressedReason)
		}
		expectGate(t, h.frontOnly(1), ghub.LaneOutcomeDeferred, ReasonNotAtFront)
	}
	if rec := h.record(); rec.Front != nil || len(rec.Waiting) != 0 || rec.LastExit != nil {
		t.Fatalf("lane = %+v, want suppressed PRs never recorded", rec)
	}
	if len(h.events) != 0 || len(beyondRules(h.f.calls)) != 0 {
		t.Fatalf("suppressed PRs audited %+v / read GitHub %v", h.events, h.f.calls)
	}

	expectGate(t, h.gateVia(3, ghub.PRAuditPathQueue), ghub.LaneOutcomeMerged, "merged")
	if len(h.f.merges) != 1 || h.f.merges[0] != "3:h3" || len(h.f.updates) != 0 {
		t.Fatalf("merges = %v updates = %v, want only the queued #3", h.f.merges, h.f.updates)
	}
	if enters := h.eventsFor(ActionFrontEnter); len(enters) != 1 || enters[0].PR != 3 {
		t.Fatalf("front enters = %+v, want only #3", enters)
	}
	if want := []int{1, 2, 1, 2, 1, 2}; fmt.Sprint(g.checks) != fmt.Sprint(want) {
		t.Fatalf("write checks = %v, want %v (never the queued PR, never front-only)", g.checks, want)
	}
}

// A front admitted while its path could write, whose writes are refused
// later, leaves on its path's next call, once: the queued waiter is
// promoted and merges, and further calls change nothing.
func TestFrontWhosePathLosesWriteLeavesOnce(t *testing.T) {
	h, g := newWriteCheckHarness(t)
	p := h.f.addPR(1, "h1")
	h.f.setRun(p.Head, "build", "in_progress", "")
	h.f.addPR(3, "h3")
	expectGate(t, h.gateVia(1, ghub.PRAuditPathSweep), ghub.LaneOutcomeWaiting, "build (in_progress)")
	expectGate(t, h.gateVia(3, ghub.PRAuditPathQueue), ghub.LaneOutcomeDeferred, ReasonNotAtFront)

	g.suppressed[1] = true
	res := h.gateVia(1, ghub.PRAuditPathSweep)
	expectGate(t, res, ghub.LaneOutcomeRefused, suppressedReason)
	rec := h.record()
	if rec.Front == nil || rec.Front.PR != 3 || len(rec.Waiting) != 0 {
		t.Fatalf("lane = %+v, want the queued #3 promoted", rec)
	}
	if rec.LastExit == nil || rec.LastExit.PR != 1 || rec.LastExit.Reason != res.Reason {
		t.Fatalf("last exit = %+v, want #1 with %q", rec.LastExit, res.Reason)
	}
	events := len(h.events)
	if exits := h.eventsFor(ActionFrontExit); len(exits) != 1 || exits[0].PR != 1 {
		t.Fatalf("front exits = %+v", exits)
	}
	if refusals := h.eventsFor(ActionRefusal); len(refusals) == 0 || refusals[len(refusals)-1].Reason != res.Reason {
		t.Fatalf("refusals = %+v, want the suppression audited", refusals)
	}
	for i := 0; i < 3; i++ {
		expectGate(t, h.gateVia(1, ghub.PRAuditPathSweep), ghub.LaneOutcomeRefused, suppressedReason)
	}
	if len(h.events) != events || h.frontPR() != 3 {
		t.Fatalf("repeat calls churned the lane: %+v", h.events[events:])
	}
	expectGate(t, h.gateVia(3, ghub.PRAuditPathQueue), ghub.LaneOutcomeMerged, "merged")
	if len(h.f.merges) != 1 || h.f.merges[0] != "3:h3" {
		t.Fatalf("merges = %v", h.f.merges)
	}
}

// An admitted waiter is deferred without any GitHub call, as before; it is
// checked again only once it reaches the front.
func TestAdmittedWaiterIsCheckedAgainAtTheFront(t *testing.T) {
	h, g := newWriteCheckHarness(t)
	p := h.f.addPR(1, "h1")
	h.f.setRun(p.Head, "build", "in_progress", "")
	h.f.addPR(2, "h2")
	expectGate(t, h.gateVia(1, ghub.PRAuditPathSweep), ghub.LaneOutcomeWaiting, "")
	expectGate(t, h.gateVia(2, ghub.PRAuditPathSweep), ghub.LaneOutcomeDeferred, ReasonNotAtFront)
	g.suppressed[2] = true
	calls, checks := len(h.f.calls), len(g.checks)
	for i := 0; i < 3; i++ {
		expectGate(t, h.gateVia(2, ghub.PRAuditPathSweep), ghub.LaneOutcomeDeferred, ReasonNotAtFront)
	}
	if extra := beyondRules(h.f.calls[calls:]); len(extra) != 0 || len(g.checks) != checks {
		t.Fatalf("waiter cost GitHub calls %v / checks %v", extra, g.checks[checks:])
	}

	h.f.pass(p.Head)
	expectGate(t, h.gateVia(1, ghub.PRAuditPathSweep), ghub.LaneOutcomeMerged, "merged")
	if h.frontPR() != 2 {
		t.Fatalf("front = #%d, want #2 promoted", h.frontPR())
	}
	expectGate(t, h.gateVia(2, ghub.PRAuditPathSweep), ghub.LaneOutcomeRefused, suppressedReason)
	if rec := h.record(); rec.Front != nil || len(rec.Waiting) != 0 || len(h.f.merges) != 1 {
		t.Fatalf("lane = %+v merges = %v, want #2 gone without a merge", rec, h.f.merges)
	}
}

// Solicited paths (lgtm queue, hive-merge relay, dashboard) are never asked:
// their writes are explicitly requested, so a front on them still updates
// its branch and merges exactly as before.
func TestSolicitedFrontStillUpdatesAndMerges(t *testing.T) {
	for _, path := range []string{ghub.PRAuditPathQueue, ghub.PRAuditPathRelay, ghub.PRAuditPathDashboard} {
		t.Run(path, func(t *testing.T) {
			h, g := newWriteCheckHarness(t)
			p := h.f.addPR(1, "h1")
			h.f.contained[p.Head] = "old-tip"
			g.suppressed[1] = true

			expectGate(t, h.gateVia(1, path), ghub.LaneOutcomeUpdated, "merged the tip in")
			h.f.pass(p.Head)
			expectGate(t, h.gateVia(1, path), ghub.LaneOutcomeMerged, "merged")
			if len(h.f.updates) != 1 || len(h.f.merges) != 1 || h.f.merges[0] != "1:"+p.Head {
				t.Fatalf("updates = %v merges = %v", h.f.updates, h.f.merges)
			}
			if len(g.checks) != 0 {
				t.Fatalf("solicited path was write-checked: %v", g.checks)
			}
		})
	}
}

// An unknown answer (the PR could not be read) never refuses: the PR enters
// as before, and the write itself is still contained.
func TestUnknownWriteCheckDoesNotRefuse(t *testing.T) {
	h, g := newWriteCheckHarness(t)
	h.f.addPR(1, "h1")
	g.err = errors.New("reading writeback target: boom")
	expectGate(t, h.gateVia(1, ghub.PRAuditPathSweep), ghub.LaneOutcomeMerged, "merged")
}

// RESTGitHub.PathCanWrite asks the containment its UpdateBranch and Merge
// apply, without writing: an unlabeled contributor PR at L6 is refused, an
// opted-in one is allowed, and a solicited path is not asked about the item.
func TestRESTPathCanWriteMatchesTheLaneWrites(t *testing.T) {
	for _, tc := range []struct {
		name      string
		labels    string
		solicited bool
		wantErr   bool
		wantGets  int
	}{
		{"unlabeled contributor PR", `[]`, false, true, 1},
		{"opted-in contributor PR", `[{"name":"hive-review"}]`, false, false, 1},
		{"solicited path", `[]`, true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gets, writes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				gets++
				_, _ = w.Write([]byte(`{"number":42,"user":{"login":"bob-test7777"},"labels":` + tc.labels + `,"pull_request":{}}`))
			}))
			defer srv.Close()
			client := ghub.NewClientForTest(srv.URL, "org", []string{"repo"}, nil)
			yes := true
			gate := writeback.New(writeback.Config{Unsolicited: writeback.UnsolicitedConfig{Backfill: &yes}},
				writeback.Deps{LevelFor: func(string) int { return 6 }}, "")
			client.SetWritebackGate(gate)
			api := &RESTGitHub{Client: func() *ghub.Client { return client }}
			ctx := context.Background()
			if tc.solicited {
				ctx = context.WithValue(ctx, mergeMethodKey{}, &mergeMethod{solicited: true})
			}
			err := api.PathCanWrite(ctx, "org/repo", 42)
			if got := errors.Is(err, writeback.ErrSuppressed); got != tc.wantErr {
				t.Fatalf("PathCanWrite = %v, want suppressed %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "hive-review") {
				t.Fatalf("refusal %q does not name the opt-in label", err)
			}
			if writes != 0 || gets != tc.wantGets {
				t.Fatalf("gets = %d writes = %d, want %d reads and no write", gets, writes, tc.wantGets)
			}
			var _ PathWriteChecker = api
		})
	}
}
