package automerge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCurrentBaseGate(t *testing.T) {
	const head = "sha-head"
	cases := []struct {
		name          string
		optIn         bool
		behindBy      int
		compareStatus int
		updateAllowed bool
		wantStop      bool
		wantReason    string
		wantErr       bool
		wantCompares  int
		wantUpdates   int
		wantUpdated   bool
	}{
		{name: "off by default", optIn: false, behindBy: 3, updateAllowed: true},
		{name: "up to date merges", optIn: true, behindBy: 0, updateAllowed: true, wantCompares: 1},
		{name: "behind updates branch", optIn: true, behindBy: 2, updateAllowed: true, wantStop: true, wantReason: "check-pending", wantCompares: 1, wantUpdates: 1, wantUpdated: true},
		{name: "behind without update budget holds", optIn: true, behindBy: 2, updateAllowed: false, wantStop: true, wantReason: "behind", wantCompares: 1},
		{name: "compare failure fails closed", optIn: true, compareStatus: http.StatusInternalServerError, updateAllowed: true, wantStop: true, wantReason: autoMergeReasonCurrentBaseCheck, wantErr: true, wantCompares: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			compares, updates := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/main..."+head:
					compares++
					if tc.compareStatus != 0 {
						w.WriteHeader(tc.compareStatus)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": tc.behindBy, "ahead_by": 1})
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
					updates++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if got, _ := body["expected_head_sha"].(string); got != head {
						t.Errorf("update-branch expected_head_sha = %q, want %q", got, head)
					}
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.requireCurrentBase = func(repo string) bool {
				return tc.optIn && strings.EqualFold(repo, "acme/widget")
			}
			event, reason, stop, err := c.currentBaseGate(context.Background(), "acme/widget", "acme", "widget", 7, "main", head, tc.updateAllowed, "behind", false)
			if stop != tc.wantStop || reason != tc.wantReason {
				t.Fatalf("stop, reason = %v, %q; want %v, %q", stop, reason, tc.wantStop, tc.wantReason)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if event.BranchUpdated != tc.wantUpdated {
				t.Fatalf("BranchUpdated = %v, want %v", event.BranchUpdated, tc.wantUpdated)
			}
			mu.Lock()
			defer mu.Unlock()
			if compares != tc.wantCompares || updates != tc.wantUpdates {
				t.Fatalf("compares, updates = %d, %d; want %d, %d", compares, updates, tc.wantCompares, tc.wantUpdates)
			}
		})
	}
}

// TestCurrentBaseGateUpdateUnavailableFallback pins the inherited-default guard
// (hivecommons/hive#15335): when the App cannot update a behind head — a fork
// without maintainer edits, or a same-repo 403 from branch protection — a
// repo gated only by the default merges the head as tested and WARNs once
// per PR; a repo explicitly listed in auto_merge.require_current_base still
// parks it.
func TestCurrentBaseGateUpdateUnavailableFallback(t *testing.T) {
	const head = "sha-head"
	cases := []struct {
		name         string
		fork         bool
		updateStatus int
		updateBody   string
		strict       bool
		wantStop     bool
		wantReason   string
		wantWarns    int
	}{
		{name: "fork not editable falls back", fork: true, updateStatus: http.StatusForbidden, updateBody: `{"message":"Resource not accessible by integration"}`, wantWarns: 1},
		{name: "fork maintainer edits off falls back", fork: true, updateStatus: http.StatusUnprocessableEntity, updateBody: `{"message":"Update is not a fast forward; maintainer edits disabled"}`, wantWarns: 1},
		{name: "same-repo forbidden falls back", fork: false, updateStatus: http.StatusForbidden, updateBody: `{"message":"Resource not accessible by integration"}`, wantWarns: 1},
		{name: "strict repo still parks fork", fork: true, strict: true, updateStatus: http.StatusForbidden, updateBody: `{"message":"Resource not accessible by integration"}`, wantStop: true, wantReason: autoMergeReasonForkNotEditable},
		{name: "conflict never falls back", fork: false, updateStatus: http.StatusUnprocessableEntity, updateBody: `{"message":"merge conflict between base and head"}`, wantStop: true, wantReason: "conflicting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/main..."+head:
					_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": 2, "ahead_by": 1})
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
					w.WriteHeader(tc.updateStatus)
					_, _ = w.Write([]byte(tc.updateBody))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			var logs strings.Builder
			c := newAutoMergeSweepClient(srv.URL)
			c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			c.requireCurrentBase = func(string) bool { return true }
			c.requireCurrentBaseStrict = func(string) bool { return tc.strict }
			for tick := 0; tick < 2; tick++ {
				_, reason, stop, err := c.currentBaseGate(context.Background(), "acme/widget", "acme", "widget", 7, "main", head, true, "behind", tc.fork)
				if err != nil {
					t.Fatalf("tick %d: err = %v", tick, err)
				}
				if stop != tc.wantStop || reason != tc.wantReason {
					t.Fatalf("tick %d: stop, reason = %v, %q; want %v, %q", tick, stop, reason, tc.wantStop, tc.wantReason)
				}
			}
			if got := strings.Count(logs.String(), currentBaseFallbackMessage); got != tc.wantWarns {
				t.Fatalf("fallback WARN lines over two ticks = %d, want %d (one per PR, not per sweep):\n%s", got, tc.wantWarns, logs.String())
			}
		})
	}
}

func TestCurrentBaseGateNilEngine(t *testing.T) {
	var c *Engine
	if c.requireCurrentBaseFor("acme/widget") {
		t.Fatal("nil engine must not require a current base")
	}
}

// currentBaseLaneAPI drives trySweepSelfAuthoredPR against a one-PR GitHub
// API whose base branch is a real ref ("main"), so the current-base gate has
// something to compare against. It counts the compare, update-branch and
// merge calls so a test can pin which of them a lane spends on a head.
type currentBaseLaneAPI struct {
	mu          sync.Mutex
	statusState string // combined-status / check-run state for the head
	behindBy    int
	mergeQueue  bool // the merge call is refused as merge-queue-only
	compares    int
	updates     int
	merges      int
}

func (a *currentBaseLaneAPI) counts() (compares, updates, merges int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.compares, a.updates, a.merges
}

func newCurrentBaseLaneServer(t *testing.T, a *currentBaseLaneAPI) *httptest.Server {
	t.Helper()
	const head = "sha7"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":          7,
				"state":           "open",
				"title":           "current-base lane fixture",
				"mergeable_state": "clean",
				"mergeable":       true,
				"user":            map[string]string{"login": testHiveAppBotLogin},
				"head":            map[string]any{"sha": head, "repo": map[string]string{"full_name": "acme/widget"}},
				"base":            map[string]any{"ref": "main", "repo": map[string]string{"full_name": "acme/widget"}},
				"labels":          []map[string]string{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]string{"sha": "base1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main/protection/required_status_checks":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Branch not protected"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/rules/branches/main":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/base1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0, "statuses": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/"+head+"/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state":       a.statusState,
				"total_count": 1,
				"statuses":    []map[string]string{{"context": "ci/build", "state": a.statusState}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/"+head+"/check-runs":
			runs := []map[string]string{}
			switch a.statusState {
			case "success":
				runs = append(runs, map[string]string{"name": "build", "status": "completed", "conclusion": "success"})
			case "failure":
				runs = append(runs, map[string]string{"name": "build", "status": "completed", "conclusion": "failure"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/actions/runs":
			// A pending head may be probed for approval-held workflow runs
			// (automerge_held_runs.go); none are held here.
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/main..."+head:
			a.compares++
			_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": a.behindBy, "ahead_by": 1})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
			a.updates++
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
			a.merges++
			if a.mergeQueue {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": mergeQueueRequiredTestMessage})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "merge7"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestTrySweepSelfAuthoredPRCurrentBaseGateAfterGreen pins the placement of
// the current-base gate in the self-authored lane: it runs only once the head
// is green, so a red or pending head — one an agent is still fixing — is
// never rebased under them, and a stale green head is updated instead of
// merged. It also pins the budget handshake with the sweep loop: the update
// comes back as a BranchUpdated event (counted against
// selfAuthoredSweepMaxBranchUpdates) and an exhausted budget holds the PR
// with autoMergeReasonBehindBudgetExhausted rather than updating it.
func TestTrySweepSelfAuthoredPRCurrentBaseGateAfterGreen(t *testing.T) {
	cases := []struct {
		name          string
		optIn         bool
		statusState   string
		behindBy      int
		updateAllowed bool
		wantReason    string
		wantCIKind    ciReasonKind
		wantUpdated   bool
		wantMergeSHA  string
		wantCompares  int
		wantUpdates   int
		wantMerges    int
	}{
		{name: "green current head merges", optIn: true, statusState: "success", behindBy: 0, updateAllowed: true, wantMergeSHA: "merge7", wantCompares: 1, wantMerges: 1},
		{name: "green behind head is updated and counted against the budget", optIn: true, statusState: "success", behindBy: 2, updateAllowed: true, wantReason: "check-pending", wantUpdated: true, wantCompares: 1, wantUpdates: 1},
		{name: "green behind head holds when the budget is spent", optIn: true, statusState: "success", behindBy: 2, updateAllowed: false, wantReason: autoMergeReasonBehindBudgetExhausted, wantCompares: 1},
		{name: "pending behind head is left alone", optIn: true, statusState: "pending", behindBy: 2, updateAllowed: true, wantCIKind: ciReasonPending},
		{name: "red behind head is left alone", optIn: true, statusState: "failure", behindBy: 2, updateAllowed: true, wantCIKind: ciReasonRed},
		{name: "opted-out repo merges a behind head without comparing", optIn: false, statusState: "success", behindBy: 2, updateAllowed: true, wantMergeSHA: "merge7", wantMerges: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &currentBaseLaneAPI{statusState: tc.statusState, behindBy: tc.behindBy}
			srv := newCurrentBaseLaneServer(t, api)
			defer srv.Close()
			c := newAutoMergeSweepClient(srv.URL)
			c.requireCurrentBase = func(repo string) bool { return tc.optIn && strings.EqualFold(repo, "acme/widget") }

			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, tc.updateAllowed)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantCIKind != ciReasonNone {
				if got := classifyCIReason(reason); got != tc.wantCIKind {
					t.Fatalf("reason = %q classified %v, want %v", reason, got, tc.wantCIKind)
				}
			} else if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if event.BranchUpdated != tc.wantUpdated || event.MergeSHA != tc.wantMergeSHA {
				t.Fatalf("event = %+v, want BranchUpdated=%v MergeSHA=%q", event, tc.wantUpdated, tc.wantMergeSHA)
			}
			compares, updates, merges := api.counts()
			if compares != tc.wantCompares || updates != tc.wantUpdates || merges != tc.wantMerges {
				t.Fatalf("compares, updates, merges = %d, %d, %d; want %d, %d, %d", compares, updates, merges, tc.wantCompares, tc.wantUpdates, tc.wantMerges)
			}
		})
	}
}

// TestContributorLaneCurrentBaseGateAfterGreen pins the placement of the
// current-base gate in the contributor lane: a behind head is only updated
// once it is green, soaked and otherwise mergeable, so a red head or one still
// soaking is never rebased under its author, and a relay evaluation (which
// may not touch the branch) holds with "behind" instead of updating.
func TestContributorLaneCurrentBaseGateAfterGreen(t *testing.T) {
	cases := []struct {
		name         string
		optIn        bool
		headStatus   string
		behindBy     int
		soaked       bool
		relay        bool
		wantReason   string
		wantCIKind   ciReasonKind
		wantSoaking  bool
		wantCompares int
		wantUpdates  int
		wantMerges   int
	}{
		{name: "green soaked current head merges", optIn: true, behindBy: 0, soaked: true, wantCompares: 1, wantMerges: 1},
		{name: "green soaked behind head is updated instead of merged", optIn: true, behindBy: 2, soaked: true, wantReason: "check-pending", wantCompares: 1, wantUpdates: 1},
		{name: "relay evaluation of a behind head holds without updating", optIn: true, behindBy: 2, soaked: true, relay: true, wantReason: "behind", wantCompares: 1},
		{name: "red behind head is left alone", optIn: true, headStatus: "failure", behindBy: 2, soaked: true, wantCIKind: ciReasonRed},
		{name: "soaking behind head is left alone until soaked", optIn: true, behindBy: 2, soaked: false, wantSoaking: true},
		{name: "opted-out repo merges a behind head without comparing", optIn: false, behindBy: 2, soaked: true, wantMerges: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.baseRef = "main"
			fx.headStatus = tc.headStatus
			fx.behindBy = tc.behindBy
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)}
			opts := Options{RequireCurrentBase: func(repo string) bool { return tc.optIn && strings.EqualFold(repo, "acme/widget") }}
			if tc.soaked {
				opts.ContributorSoak = soakedTracker(clock)
			}
			engine := newContributorLaneEngine(api.URL, policy, clock, opts)

			var reason string
			var err error
			if tc.relay {
				_, reason, err = engine.evaluateContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy, true)
			} else {
				_, reason, err = engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			}
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.wantCIKind != ciReasonNone:
				if got := classifyCIReason(reason); got != tc.wantCIKind {
					t.Fatalf("reason = %q classified %v, want %v", reason, got, tc.wantCIKind)
				}
			case tc.wantSoaking:
				if !strings.HasPrefix(reason, contributorLaneReasonSoaking) {
					t.Fatalf("reason = %q, want a soaking reason", reason)
				}
			default:
				if reason != tc.wantReason {
					t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
				}
			}
			if fx.compares != tc.wantCompares || fx.updates != tc.wantUpdates || fx.merges != tc.wantMerges {
				t.Fatalf("compares, updates, merges = %d, %d, %d; want %d, %d, %d", fx.compares, fx.updates, fx.merges, tc.wantCompares, tc.wantUpdates, tc.wantMerges)
			}
		})
	}
}

// TestCurrentBaseGateMergeQueueBase pins that the gate never merges the base
// into a behind head on a base that refused a direct merge as
// merge-queue-only (projectbluefin/finpilot#599): it lets the PR through to
// the merge-queue hold instead, without an update-branch call, and marks the
// base held so the sweep does not probe it with that behind head. A base
// whose last refusal is older than mergeQueueBaseRefreshHold is updated as
// before.
func TestCurrentBaseGateMergeQueueBase(t *testing.T) {
	const head = "sha-head"
	t0 := time.Date(2026, 10, 28, 9, 31, 0, 0, time.UTC)
	cases := []struct {
		name          string
		refusedAgo    time.Duration // 0: the base never refused
		updateAllowed bool
		wantStop      bool
		wantReason    string
		wantUpdates   int
		wantRequired  bool
	}{
		{name: "normal base is updated", updateAllowed: true, wantStop: true, wantReason: "check-pending", wantUpdates: 1},
		{name: "merge-queue base inside the old recheck is not updated", refusedAgo: 10 * time.Minute, updateAllowed: true, wantRequired: true},
		{name: "merge-queue base past the old recheck is not updated", refusedAgo: 101 * time.Minute, updateAllowed: true, wantRequired: true},
		{name: "merge-queue base without update budget is not held behind", refusedAgo: 101 * time.Minute, updateAllowed: false, wantRequired: true},
		{name: "merge-queue refusal past the refresh hold updates again", refusedAgo: mergeQueueBaseRefreshHold, updateAllowed: true, wantStop: true, wantReason: "check-pending", wantUpdates: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			compares, updates := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/main..."+head:
					compares++
					_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": 2, "ahead_by": 1})
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
					updates++
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.requireCurrentBase = func(string) bool { return true }
			now := t0
			c.now = func() time.Time { return now }
			if tc.refusedAgo > 0 {
				now = t0.Add(-tc.refusedAgo)
				c.noteMergeQueueBase("acme", "widget", "main")
				now = t0
			}
			_, reason, stop, err := c.currentBaseGate(context.Background(), "acme/widget", "acme", "widget", 7, "main", head, tc.updateAllowed, "behind", false)
			if err != nil {
				t.Fatal(err)
			}
			if stop != tc.wantStop || reason != tc.wantReason {
				t.Fatalf("stop, reason = %v, %q; want %v, %q", stop, reason, tc.wantStop, tc.wantReason)
			}
			mu.Lock()
			defer mu.Unlock()
			if compares != 1 || updates != tc.wantUpdates {
				t.Fatalf("compares, updates = %d, %d; want 1, %d", compares, updates, tc.wantUpdates)
			}
			if got := c.mergeQueueBaseKnown("acme", "widget", "main"); got != tc.wantRequired {
				t.Fatalf("merge-queue base known after the gate = %v, want %v", got, tc.wantRequired)
			}
		})
	}
}

// TestSelfAuthoredLaneNoBaseRefreshOnMergeQueueBase replays
// projectbluefin/finpilot#599 through the self-authored lane: a green
// current head is refused as merge-queue-only, then the base keeps moving.
// Every later sweep, hours past the 30-minute merge-queue recheck, holds the
// PR at merge-queue-required without an update-branch call (each one used to
// reset the review) and without a direct-merge probe of the behind head. A
// head that is current again re-probes the base once no behind head was held
// for mergeQueueBaseRecheck, and a probe that is refused again re-arms the
// hold, so the hold lasts while the refusals continue.
func TestSelfAuthoredLaneNoBaseRefreshOnMergeQueueBase(t *testing.T) {
	api := &currentBaseLaneAPI{statusState: "success", mergeQueue: true}
	srv := newCurrentBaseLaneServer(t, api)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.requireCurrentBase = func(repo string) bool { return strings.EqualFold(repo, "acme/widget") }
	t0 := time.Date(2026, 10, 28, 9, 31, 0, 0, time.UTC)
	now := t0
	c.now = func() time.Time { return now }

	sweep := func(step string, at time.Duration, behindBy int, wantReason string, wantUpdates, wantMerges int) {
		t.Helper()
		now = t0.Add(at)
		api.mu.Lock()
		api.behindBy = behindBy
		api.mu.Unlock()
		_, beforeUpdates, beforeMerges := api.counts()
		event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		_, updates, merges := api.counts()
		if reason != wantReason || event.BranchUpdated != (wantUpdates > 0) {
			t.Fatalf("%s: reason = %q, BranchUpdated = %v; want %q, %v", step, reason, event.BranchUpdated, wantReason, wantUpdates > 0)
		}
		if updates-beforeUpdates != wantUpdates || merges-beforeMerges != wantMerges {
			t.Fatalf("%s: updates, merges = %d, %d; want %d, %d", step, updates-beforeUpdates, merges-beforeMerges, wantUpdates, wantMerges)
		}
	}

	sweep("current head is refused by the merge queue", 0, 0, autoMergeReasonMergeQueueRequired, 0, 1)
	sweep("base moved inside the recheck", 15*time.Minute, 2, autoMergeReasonMergeQueueRequired, 0, 0)
	sweep("base moved 1h41m after the refusal (the 11:13Z refresh)", 101*time.Minute, 3, autoMergeReasonMergeQueueRequired, 0, 0)
	sweep("base moved 5h45m after the refusal (the 15:16Z refresh)", 345*time.Minute, 4, autoMergeReasonMergeQueueRequired, 0, 0)
	sweep("current head right after a hold is not probed", 350*time.Minute, 0, autoMergeReasonMergeQueueRequired, 0, 0)
	sweep("current head re-probes once no hold for the recheck", 345*time.Minute+mergeQueueBaseRecheck, 0, autoMergeReasonMergeQueueRequired, 0, 1)
	reprobe := 345*time.Minute + mergeQueueBaseRecheck
	sweep("behind head a day after the first refusal is still held", reprobe+20*time.Hour, 2, autoMergeReasonMergeQueueRequired, 0, 0)
	sweep("no refusal for the refresh hold: the base is updated again", reprobe+mergeQueueBaseRefreshHold, 2, "check-pending", 1, 0)
}

// TestSelfAuthoredLaneMergeQueueRuleRemoved pins the way out of the hold:
// once the queue rule is gone, the probe of a current head merges it, and a
// behind head on that base is updated again after mergeQueueBaseRefreshHold
// at the latest, never merged behind its base.
func TestSelfAuthoredLaneMergeQueueRuleRemoved(t *testing.T) {
	api := &currentBaseLaneAPI{statusState: "success", behindBy: 2}
	srv := newCurrentBaseLaneServer(t, api)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.requireCurrentBase = func(repo string) bool { return strings.EqualFold(repo, "acme/widget") }
	t0 := time.Date(2026, 10, 28, 9, 31, 0, 0, time.UTC)
	now := t0
	c.now = func() time.Time { return now }
	c.noteMergeQueueBase("acme", "widget", "main")

	for _, at := range []time.Duration{time.Hour, 6 * time.Hour, 23 * time.Hour} {
		now = t0.Add(at)
		_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
		if err != nil || reason != autoMergeReasonMergeQueueRequired {
			t.Fatalf("at +%s: reason, err = %q, %v; want %q", at, reason, err, autoMergeReasonMergeQueueRequired)
		}
	}
	if _, updates, merges := api.counts(); updates != 0 || merges != 0 {
		t.Fatalf("held behind head: updates, merges = %d, %d; want 0, 0", updates, merges)
	}
	now = t0.Add(mergeQueueBaseRefreshHold)
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
	if err != nil || reason != "check-pending" || !event.BranchUpdated {
		t.Fatalf("after the refresh hold: reason, BranchUpdated, err = %q, %v, %v; want check-pending, true", reason, event.BranchUpdated, err)
	}
	if _, updates, merges := api.counts(); updates != 1 || merges != 0 {
		t.Fatalf("after the refresh hold: updates, merges = %d, %d; want 1, 0", updates, merges)
	}
}
