package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/reach"
)

// fakeReachPRSource is an in-memory reach.PRSource.
type fakeReachPRSource struct {
	prs map[int]reach.PRInfo
	err error
}

func (f *fakeReachPRSource) MergedPR(_ context.Context, number int) (reach.PRInfo, error) {
	if f.err != nil {
		return reach.PRInfo{}, f.err
	}
	pr, ok := f.prs[number]
	if !ok {
		return reach.PRInfo{}, fmt.Errorf("PR %d is not merged", number)
	}
	return pr, nil
}

func (f *fakeReachPRSource) RecentMergedPRs(_ context.Context, limit int) ([]reach.PRInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []reach.PRInfo
	for _, pr := range f.prs {
		if len(out) >= limit {
			break
		}
		out = append(out, pr)
	}
	return out, nil
}

// fakeReachAncestry answers from an explicit containment table;
// self-ancestry implied.
type fakeReachAncestry struct{ pairs map[[2]string]bool }

func (f *fakeReachAncestry) IsAncestor(ancestor, descendant string) (bool, error) {
	if ancestor == descendant {
		return true, nil
	}
	return f.pairs[[2]string{ancestor, descendant}], nil
}

// TestReachEndpointRequiresAdmin: an unauthenticated GET /api/reach must be
// refused by the SAME gate, with the SAME failure, as the neighboring
// protected fleet-status route (GET /api/saas/cluster-health) — and it must
// leak no reach data even when a data source IS configured. If the
// requireAdmin wrapper were removed from the route registration, the request
// would reach handleReach and answer 200 with reach data, so every assertion
// below fails (verified by mutating the registration during development).
func TestReachEndpointRequiresAdmin(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	// Data IS available — the gate, not a missing dependency, must refuse.
	mergedAt := time.Now().Add(-48 * time.Hour)
	srv.reachPRSource = &fakeReachPRSource{prs: map[int]reach.PRInfo{
		3994: {Number: 3994, MergeCommit: "cafe123", MergedAt: mergedAt, Files: []string{"v2/pkg/hub/saas.go"}},
	}}
	srv.reachReporter = &reach.StubReachReporter{Reports: map[string][]reach.ComponentReach{
		"hive-secret": {{Component: "hub", Commit: "beef456", SpansTotal: 5, FirstSeen: mergedAt.Add(time.Hour)}},
	}}
	srv.reachAncestry = &fakeReachAncestry{pairs: map[[2]string]bool{{"cafe123", "beef456"}: true}}

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil) // GET is CSRF-safe; no cookie, no bearer
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		return w
	}

	reachResp := get("/api/reach?pr=3994")
	neighborResp := get("/api/saas/cluster-health")

	if reachResp.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated /api/reach: got %d, want %d", reachResp.Code, http.StatusForbidden)
	}
	// EXACT parity with the neighboring requireAdmin route: same status,
	// same body.
	if reachResp.Code != neighborResp.Code || reachResp.Body.String() != neighborResp.Body.String() {
		t.Errorf("auth failure differs from neighboring admin route:\n /api/reach: %d %q\n cluster-health: %d %q",
			reachResp.Code, reachResp.Body.String(), neighborResp.Code, neighborResp.Body.String())
	}
	// And no data escaped around the gate.
	for _, marker := range []string{"reach_hives", "generated_at", "hive-secret", "cafe123"} {
		if strings.Contains(reachResp.Body.String(), marker) {
			t.Errorf("unauthenticated response leaked reach data (%q): %s", marker, reachResp.Body.String())
		}
	}
}

// TestHandleReachNoSource: with no GitHub PR source wired (hub without
// credentials), the handler reports 503 — never fabricated data.
func TestHandleReachNoSource(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/reach", nil)
	w := httptest.NewRecorder()
	srv.handleReach(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleReachBadParams: malformed queries are 400s, not upstream calls.
func TestHandleReachBadParams(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.reachPRSource = &fakeReachPRSource{err: errors.New("must not be called")}
	for _, q := range []string{"?pr=abc", "?pr=-1", "?recent=0", "?recent=99999", "?recent=x"} {
		req := httptest.NewRequest("GET", "/api/reach"+q, nil)
		w := httptest.NewRecorder()
		srv.handleReach(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400; body=%s", q, w.Code, w.Body.String())
		}
	}
}

// TestHandleReachSinglePR exercises the full join through the handler: D3
// attribution with coverage, ancestry-gated reach, first-execution latency,
// and the D4 window label derived from what the fleet reports running.
func TestHandleReachSinglePR(t *testing.T) {
	mergedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	firstRun := mergedAt.Add(90 * time.Minute)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.reachPRSource = &fakeReachPRSource{prs: map[int]reach.PRInfo{
		3994: {
			Number:      3994,
			MergeCommit: "cafe123",
			MergedAt:    mergedAt,
			Files:       []string{"v2/pkg/hub/saas.go", "v2/docs/design/pr-reach-telemetry.md"},
		},
	}}
	srv.reachReporter = &reach.StubReachReporter{Reports: map[string][]reach.ComponentReach{
		// Runs a build containing the PR and executed the touched component.
		"hive-a": {{Component: "hub", Commit: "beef456", SpansTotal: 7, FirstSeen: firstRun, LastSeen: firstRun}},
		// Still on a pre-PR build: contributes a deploy window, not reach.
		"hive-b": {{Component: "hub", Commit: "old0001", SpansTotal: 9, FirstSeen: mergedAt.Add(-time.Hour), LastSeen: firstRun}},
	}}
	srv.reachAncestry = &fakeReachAncestry{pairs: map[[2]string]bool{
		{"cafe123", "beef456"}: true,
	}}

	req := httptest.NewRequest("GET", "/api/reach?pr=3994", nil)
	w := httptest.NewRecorder()
	srv.handleReach(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		HivesReporting  int                   `json:"hives_reporting"`
		DeployedCommits []string              `json:"deployed_commits"`
		Reports         []reach.PRReachReport `json:"reports"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, w.Body.String())
	}
	if resp.HivesReporting != 2 {
		t.Errorf("HivesReporting = %d, want 2", resp.HivesReporting)
	}
	if len(resp.Reports) != 1 {
		t.Fatalf("Reports = %d, want 1", len(resp.Reports))
	}
	report := resp.Reports[0]
	if got, want := report.ReachHives, []string{"hive-a"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("ReachHives = %v, want %v", got, want)
	}
	if !report.Deployed {
		t.Error("Deployed = false, want true")
	}
	// D3: coverage rides along, unattributable share intact (1 of 2 files).
	if report.Attribution.Coverage != 0.5 {
		t.Errorf("Coverage = %v, want 0.5", report.Attribution.Coverage)
	}
	if len(report.Attribution.UnattributableFiles) != 1 {
		t.Errorf("UnattributableFiles = %v, want the docs file", report.Attribution.UnattributableFiles)
	}
	// D4: window = the deployed commit that shipped the PR.
	if report.DeployWindow != "beef456" {
		t.Errorf("DeployWindow = %q, want %q", report.DeployWindow, "beef456")
	}
	if report.FirstExecutionLatencySeconds == nil || *report.FirstExecutionLatencySeconds != (90*time.Minute).Seconds() {
		t.Errorf("FirstExecutionLatencySeconds = %v, want %v", report.FirstExecutionLatencySeconds, (90 * time.Minute).Seconds())
	}
}

// TestCompareAncestry pins the compare-API adapter to commit_order.go's
// exact semantics ("ahead"/"identical" = contained), its shared permanent
// cache, and its errors-are-not-cached discipline.
func TestCompareAncestry(t *testing.T) {
	anc := compareAncestry{logger: slog.Default()}

	// Stub the fetcher under commitOrderMu, per its contract.
	calls := 0
	commitOrderMu.Lock()
	origFetch := fetchCommitCompareStatus
	origCache := commitOrderCache
	commitOrderCache = map[commitOrderKey]bool{}
	fetchCommitCompareStatus = func(base, head string, _ *slog.Logger) (string, error) {
		calls++
		switch base + ".." + head {
		case "aaaaaaa..bbbbbbb":
			return "ahead", nil
		case "bbbbbbb..aaaaaaa":
			return "behind", nil
		case "ccccccc..ddddddd":
			return "", errors.New("transient")
		}
		return "diverged", nil
	}
	commitOrderMu.Unlock()
	defer func() {
		commitOrderMu.Lock()
		fetchCommitCompareStatus = origFetch
		commitOrderCache = origCache
		commitOrderMu.Unlock()
	}()

	if got, err := anc.IsAncestor("aaaaaaa", "bbbbbbb"); err != nil || !got {
		t.Errorf("IsAncestor(ahead) = %v, %v; want true, nil", got, err)
	}
	if got, err := anc.IsAncestor("bbbbbbb", "aaaaaaa"); err != nil || got {
		t.Errorf("IsAncestor(behind) = %v, %v; want false, nil", got, err)
	}
	// Same commit needs no API call.
	preCalls := calls
	if got, err := anc.IsAncestor("aaaaaaa", "aaaaaaa"); err != nil || !got {
		t.Errorf("IsAncestor(self) = %v, %v; want true, nil", got, err)
	}
	if calls != preCalls {
		t.Errorf("self-comparison hit the API")
	}
	// Cached: repeating the first query performs no new fetch.
	preCalls = calls
	if got, _ := anc.IsAncestor("aaaaaaa", "bbbbbbb"); !got {
		t.Error("cached answer changed")
	}
	if calls != preCalls {
		t.Error("cached pair re-fetched")
	}
	// Errors surface and are NOT cached: a retry fetches again.
	if _, err := anc.IsAncestor("ccccccc", "ddddddd"); err == nil {
		t.Error("want error from failing fetch")
	}
	preCalls = calls
	anc.IsAncestor("ccccccc", "ddddddd")
	if calls != preCalls+1 {
		t.Error("failed pair was cached instead of retried")
	}
	// Empty input is an error, never an answer.
	if _, err := anc.IsAncestor("", "bbbbbbb"); err == nil {
		t.Error("empty ancestor: want error")
	}
}

// TestHandleReachErrorDeltas (#3995, phase 2c): the handler attaches
// per-component error-rate deltas from the retention store AFTER window
// assignment, with exact numeric values — and a PR whose deploy window has no
// retained before-history answers measured=false, never a fabricated zero.
func TestHandleReachErrorDeltas(t *testing.T) {
	mergedAt := time.Now().UTC().Add(-4 * time.Hour)
	firstRun := mergedAt.Add(30 * time.Minute)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.reachPRSource = &fakeReachPRSource{prs: map[int]reach.PRInfo{
		3995: {
			Number:      3995,
			MergeCommit: "cafe123",
			MergedAt:    mergedAt,
			Files:       []string{"src/pkg/hub/reach_history.go"},
		},
	}}
	srv.reachReporter = &reach.StubReachReporter{Reports: map[string][]reach.ComponentReach{
		"hive-a": {{Component: "hub", Commit: "beef456", SpansTotal: 7, FirstSeen: firstRun, LastSeen: firstRun}},
	}}
	srv.reachAncestry = &fakeReachAncestry{pairs: map[[2]string]bool{
		{"cafe123", "beef456"}: true,
	}}

	// Retention: a mixed-fleet history — one hive still on the old build
	// (the before regime), one on the deployed build (after). Before rate
	// 10/100 = 0.10, after rate 30/100 = 0.30, delta +0.20 over 1 window.
	store, _, _ := newTestHistoryStore(t)
	now := time.Now().UTC()
	store.Append("hive-b", histReport("hub", "01dc0de", now.Add(-3*time.Hour), 100, 10), now)
	store.Append("hive-a", histReport("hub", "beef456", now.Add(-90*time.Minute), 100, 30), now)
	srv.reachHistory = store

	req := httptest.NewRequest("GET", "/api/reach?pr=3995", nil)
	w := httptest.NewRecorder()
	srv.handleReach(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	// The wire shape is part of the contract: field names are what the
	// dashboard table reads.
	for _, key := range []string{"error_deltas", "error_rate_before", "error_rate_after", "windows_compared", "measured"} {
		if !strings.Contains(w.Body.String(), key) {
			t.Errorf("response missing %q:\n%s", key, w.Body.String())
		}
	}

	var resp struct {
		Reports []reach.PRReachReport `json:"reports"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, w.Body.String())
	}
	if len(resp.Reports) != 1 {
		t.Fatalf("Reports = %d, want 1", len(resp.Reports))
	}
	report := resp.Reports[0]
	if report.DeployWindow != "beef456" {
		t.Fatalf("DeployWindow = %q, want beef456", report.DeployWindow)
	}
	if len(report.ErrorDeltas) != 1 {
		t.Fatalf("ErrorDeltas = %+v, want exactly one (component hub)", report.ErrorDeltas)
	}
	d := report.ErrorDeltas[0]
	if d.Component != "hub" || !d.Measured {
		t.Fatalf("delta = %+v, want measured for hub", d)
	}
	const eps = 1e-9
	if diff := d.ErrorRateBefore - 0.10; diff > eps || diff < -eps {
		t.Errorf("ErrorRateBefore = %v, want 0.10", d.ErrorRateBefore)
	}
	if diff := d.ErrorRateAfter - 0.30; diff > eps || diff < -eps {
		t.Errorf("ErrorRateAfter = %v, want 0.30", d.ErrorRateAfter)
	}
	if diff := d.Delta - 0.20; diff > eps || diff < -eps {
		t.Errorf("Delta = %v, want 0.20", d.Delta)
	}
	if d.WindowsCompared != 1 {
		t.Errorf("WindowsCompared = %d, want 1", d.WindowsCompared)
	}

	// Converged-fleet case (the dominant one in production): wipe the old
	// build's history so no before-side exists — the delta must come back
	// present but UNMEASURED, with zeroed numbers.
	converged, _, _ := newTestHistoryStore(t)
	converged.Append("hive-a", histReport("hub", "beef456", now.Add(-90*time.Minute), 100, 30), now)
	srv.reachHistory = converged

	w = httptest.NewRecorder()
	srv.handleReach(w, httptest.NewRequest("GET", "/api/reach?pr=3995", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("converged: got %d, body=%s", w.Code, w.Body.String())
	}
	resp.Reports = nil
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("converged: bad JSON: %v", err)
	}
	if len(resp.Reports) != 1 || len(resp.Reports[0].ErrorDeltas) != 1 {
		t.Fatalf("converged: deltas = %+v, want one entry", resp.Reports)
	}
	cd := resp.Reports[0].ErrorDeltas[0]
	if cd.Measured || cd.Delta != 0 || cd.ErrorRateBefore != 0 || cd.ErrorRateAfter != 0 || cd.WindowsCompared != 0 {
		t.Errorf("converged fleet delta = %+v, want unmeasured with zeroed numbers", cd)
	}
}
