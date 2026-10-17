package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// trailCheck is one check run or commit status on a head, as the fake
// GitHub serves it. Status checks are served under /status, check runs
// under /check-runs.
type trailCheck struct {
	name       string
	status     string // check runs: queued|in_progress|completed
	conclusion string // check runs: success|failure|...
	state      string // statuses: success|pending|failure
}

func trailCheckRun(name, status, conclusion string) trailCheck {
	return trailCheck{name: name, status: status, conclusion: conclusion}
}

// trailFixture is the fake GitHub behind PRCIRecoveryTrail: one PR, its
// commit list (oldest first, paged), the base branch's required set and
// the checks on every head.
type trailFixture struct {
	state      string
	mergedAt   string
	mergedBy   string
	closedAt   string
	baseRef    string
	protected  bool
	required   []string
	commits    []string
	pageSize   int
	checks     map[string][]trailCheck
	prStatus   int    // non-zero: GET /pulls/N answers this status
	listStatus int    // non-zero: GET /pulls/N/commits answers this status
	checksErr  string // SHA whose /status answers 500

	mu   sync.Mutex
	hits map[string]int
}

func (f *trailFixture) hit(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits == nil {
		f.hits = map[string]int{}
	}
	f.hits[key]++
}

func (f *trailFixture) hitCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

func (f *trailFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	pageSize := f.pageSize
	if pageSize <= 0 {
		pageSize = prCITrailPerPage
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		enc := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		fail := func(status int, msg string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
		}
		switch {
		case p == "/repos/o/r/pulls/7":
			f.hit("pr")
			if f.prStatus != 0 {
				fail(f.prStatus, "pull request unavailable")
				return
			}
			pr := map[string]any{
				"number": 7,
				"state":  f.state,
				"head":   map[string]any{"sha": f.commits[len(f.commits)-1], "repo": map[string]any{"full_name": "o/r"}},
				"base":   map[string]any{"ref": f.baseRef, "repo": map[string]any{"full_name": "o/r"}},
			}
			if f.mergedAt != "" {
				pr["merged_at"] = f.mergedAt
				pr["merged_by"] = map[string]any{"login": f.mergedBy}
			}
			if f.closedAt != "" {
				pr["closed_at"] = f.closedAt
			}
			enc(pr)
		case strings.HasSuffix(p, "/protection/required_status_checks"):
			f.hit("required")
			if !f.protected {
				fail(http.StatusNotFound, "Branch not protected")
				return
			}
			checks := make([]map[string]any, 0, len(f.required))
			for _, name := range f.required {
				checks = append(checks, map[string]any{"context": name, "app_id": 1})
			}
			enc(map[string]any{"strict": true, "contexts": f.required, "checks": checks})
		case p == "/repos/o/r/pulls/7/commits":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < 1 {
				page = 1
			}
			f.hit("commits:" + strconv.Itoa(page))
			if f.listStatus != 0 {
				fail(f.listStatus, "commits unavailable")
				return
			}
			start := (page - 1) * pageSize
			end := start + pageSize
			if start > len(f.commits) {
				start = len(f.commits)
			}
			if end > len(f.commits) {
				end = len(f.commits)
			}
			if end < len(f.commits) {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?per_page=%d&page=%d>; rel="next"`, r.Host, p, pageSize, page+1))
			}
			out := make([]map[string]any, 0, end-start)
			for i, sha := range f.commits[start:end] {
				date := time.Date(2026, 10, 1, 0, start+i, 0, 0, time.UTC).Format(time.RFC3339)
				out = append(out, map[string]any{
					"sha":    sha,
					"commit": map[string]any{"committer": map[string]any{"date": date}},
				})
			}
			enc(out)
		case strings.HasPrefix(p, "/repos/o/r/commits/") && strings.HasSuffix(p, "/status"):
			sha := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/o/r/commits/"), "/status")
			f.hit("status:" + sha)
			if sha == f.checksErr {
				fail(http.StatusInternalServerError, "status unavailable")
				return
			}
			statuses := []map[string]string{}
			for _, c := range f.checks[sha] {
				if c.state != "" {
					statuses = append(statuses, map[string]string{"context": c.name, "state": c.state})
				}
			}
			enc(map[string]any{"state": "pending", "total_count": len(statuses), "statuses": statuses})
		case strings.HasPrefix(p, "/repos/o/r/commits/") && strings.HasSuffix(p, "/check-runs"):
			sha := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/o/r/commits/"), "/check-runs")
			f.hit("check-runs:" + sha)
			runs := []map[string]any{}
			for i, c := range f.checks[sha] {
				if c.state != "" {
					continue
				}
				runs = append(runs, map[string]any{
					"id": i + 1, "name": c.name, "status": c.status, "conclusion": c.conclusion,
					"app": map[string]any{"id": 1},
				})
			}
			enc(map[string]any{"total_count": len(runs), "check_runs": runs})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			fail(http.StatusNotFound, "Not Found")
		}
	}))
}

func trailHeadSHAs(tr PRCITrail) []string {
	out := make([]string, 0, len(tr.Heads))
	for _, h := range tr.Heads {
		out = append(out, h.SHA)
	}
	return out
}

func trailHeadStates(tr PRCITrail) []string {
	out := make([]string, 0, len(tr.Heads))
	for _, h := range tr.Heads {
		out = append(out, h.State)
	}
	return out
}

// A merged PR whose claim head sits mid-history: the trail is the claim head
// and everything after it, each head judged on the base branch's required
// set, with the merge outcome carried over and the convergence scan counting
// evaluated commits from the PR's first up to the first green one.
func TestPRCIRecoveryTrailMergedPRFromClaimHead(t *testing.T) {
	f := &trailFixture{
		state:     "closed",
		mergedAt:  "2026-10-02T12:00:00Z",
		mergedBy:  "octocat",
		closedAt:  "2026-10-02T12:00:00Z",
		baseRef:   "v5",
		protected: true,
		required:  []string{"test", "gate"},
		commits:   []string{"c1", "c2", "c3", "c4"},
		checks: map[string][]trailCheck{
			"c1": {trailCheckRun("test", "completed", "failure"), trailCheckRun("gate", "completed", "success")},
			"c2": {
				trailCheckRun("test", "completed", "failure"),
				trailCheckRun("gate", "completed", "success"),
				trailCheckRun("docs-links", "completed", "failure"),
			},
			"c3": {trailCheckRun("test", "in_progress", ""), trailCheckRun("gate", "completed", "success")},
			"c4": {trailCheckRun("test", "completed", "success"), trailCheckRun("gate", "completed", "success")},
		},
	}
	srv := f.serve(t)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})

	tr, err := c.PRCIRecoveryTrail(context.Background(), "o/r", 7, "C2")
	if err != nil {
		t.Fatalf("PRCIRecoveryTrail: %v", err)
	}
	if tr.State != "merged" || tr.MergedBy != "octocat" {
		t.Fatalf("state/mergedBy = %q/%q, want merged/octocat", tr.State, tr.MergedBy)
	}
	wantAt := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if !tr.MergedAt.Equal(wantAt) || !tr.ClosedAt.Equal(wantAt) {
		t.Fatalf("mergedAt/closedAt = %v/%v, want %v", tr.MergedAt, tr.ClosedAt, wantAt)
	}
	if !tr.RequiredKnown || strings.Join(tr.RequiredChecks, ",") != "gate,test" {
		t.Fatalf("required = known %v %v, want known [gate test] (sorted)", tr.RequiredKnown, tr.RequiredChecks)
	}
	if got := strings.Join(trailHeadSHAs(tr), ","); got != "c2,c3,c4" {
		t.Fatalf("heads = %s, want c2,c3,c4 (claim head matched case-insensitively, then every later head)", got)
	}
	if got := strings.Join(trailHeadStates(tr), ","); got != "red,pending,green" {
		t.Fatalf("head states = %s, want red,pending,green", got)
	}
	if tr.Truncated {
		t.Fatal("Truncated = true for a 3-head trail")
	}
	claim := tr.Heads[0]
	if strings.Join(claim.RequiredRed, ",") != "test" || strings.Join(claim.NonRequiredRed, ",") != "docs-links" {
		t.Fatalf("claim head reds = required %v non-required %v, want [test] / [docs-links]", claim.RequiredRed, claim.NonRequiredRed)
	}
	if claim.At.IsZero() || !tr.Heads[2].At.After(claim.At) {
		t.Fatalf("head timestamps not carried from the commit list: %v .. %v", claim.At, tr.Heads[2].At)
	}
	if tr.Evaluated != 4 || tr.PassesToGreen != 4 || tr.ScanCapped {
		t.Fatalf("convergence = evaluated %d passes %d capped %v, want 4/4/false (green found exactly at the cap)", tr.Evaluated, tr.PassesToGreen, tr.ScanCapped)
	}
	// The heads loop and the convergence scan share one read per SHA.
	for _, sha := range f.commits {
		if n := f.hitCount("status:" + sha); n != 1 {
			t.Fatalf("GET /commits/%s/status hit %d times, want 1 (reads must be memoised across the two passes)", sha, n)
		}
	}
	if n := f.hitCount("required"); n != 1 {
		t.Fatalf("required_status_checks hit %d times, want 1", n)
	}
}

// When the claim head was force-pushed away the trail is the newest heads,
// capped at prCITrailMaxHeads with Truncated set, and a convergence scan
// that never finds green stops at PRCIConvergenceCap with ScanCapped set and
// no PassesToGreen. Heads GitHub has nothing on are skipped by the scan.
func TestPRCIRecoveryTrailForcePushedClaimHeadTruncatesAndCaps(t *testing.T) {
	commits := make([]string, 0, prCITrailMaxHeads+2)
	checks := map[string][]trailCheck{}
	for i := 1; i <= prCITrailMaxHeads+2; i++ {
		sha := fmt.Sprintf("c%d", i)
		commits = append(commits, sha)
		if i > 1 {
			checks[sha] = []trailCheck{trailCheckRun("test", "completed", "failure")}
		}
	}
	f := &trailFixture{
		state:     "open",
		baseRef:   "v5",
		protected: true,
		required:  []string{"test"},
		commits:   commits,
		checks:    checks,
	}
	srv := f.serve(t)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})

	tr, err := c.PRCIRecoveryTrail(context.Background(), "o/r", 7, "deadbeef")
	if err != nil {
		t.Fatalf("PRCIRecoveryTrail: %v", err)
	}
	if tr.State != "open" || !tr.MergedAt.IsZero() || !tr.ClosedAt.IsZero() || tr.MergedBy != "" {
		t.Fatalf("open PR outcome leaked merge/close fields: %+v", tr)
	}
	if !tr.Truncated {
		t.Fatal("Truncated = false with more heads than prCITrailMaxHeads")
	}
	want := strings.Join(commits[len(commits)-prCITrailMaxHeads:], ",")
	if got := strings.Join(trailHeadSHAs(tr), ","); got != want {
		t.Fatalf("heads = %s, want the newest %d: %s", got, prCITrailMaxHeads, want)
	}
	for _, h := range tr.Heads {
		if h.State != PRCIHeadRed {
			t.Fatalf("head %s state = %q, want red", h.SHA, h.State)
		}
	}
	if tr.Evaluated != PRCIConvergenceCap || tr.PassesToGreen != 0 || !tr.ScanCapped {
		t.Fatalf("convergence = evaluated %d passes %d capped %v, want %d/0/true", tr.Evaluated, tr.PassesToGreen, tr.ScanCapped, PRCIConvergenceCap)
	}
	// c1 carries no check at all: it is read (to learn that) but never counted.
	if n := f.hitCount("status:c1"); n != 1 {
		t.Fatalf("GET /commits/c1/status hit %d times, want 1", n)
	}
}

// The commit list is followed across Link pages so a claim head on a later
// page is still found; with an unprotected base and no configured required
// set the heads fall back to the any-check reading, where a red check that
// would be non-required elsewhere does make the head red.
func TestPRCIRecoveryTrailPaginatesCommitsAndFallsBackWithoutRequiredSet(t *testing.T) {
	f := &trailFixture{
		state:     "closed",
		closedAt:  "2026-10-03T08:00:00Z",
		baseRef:   "v5",
		protected: false,
		commits:   []string{"c1", "c2", "c3", "c4", "c5"},
		pageSize:  3,
		checks: map[string][]trailCheck{
			"c2": {{name: "test", state: "failure"}},
			"c3": {{name: "test", state: "success"}},
			"c4": {trailCheckRun("test", "completed", "success")},
			"c5": {trailCheckRun("docs-links", "completed", "failure"), {name: "tide", state: "pending"}},
		},
	}
	srv := f.serve(t)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})

	tr, err := c.PRCIRecoveryTrail(context.Background(), "o/r", 7, "c4")
	if err != nil {
		t.Fatalf("PRCIRecoveryTrail: %v", err)
	}
	if f.hitCount("commits:1") != 1 || f.hitCount("commits:2") != 1 {
		t.Fatalf("commit pages hit page1=%d page2=%d, want 1/1", f.hitCount("commits:1"), f.hitCount("commits:2"))
	}
	if tr.State != "closed" || tr.ClosedAt.IsZero() || !tr.MergedAt.IsZero() {
		t.Fatalf("closed-unmerged PR outcome = %+v, want state closed with ClosedAt only", tr)
	}
	if tr.RequiredKnown || len(tr.RequiredChecks) != 0 {
		t.Fatalf("required = known %v %v, want unknown on an unprotected base", tr.RequiredKnown, tr.RequiredChecks)
	}
	if got := strings.Join(trailHeadSHAs(tr), ","); got != "c4,c5" {
		t.Fatalf("heads = %s, want c4,c5 (claim head found on page 2)", got)
	}
	if got := strings.Join(trailHeadStates(tr), ","); got != "green,red" {
		t.Fatalf("head states = %s, want green,red (any-check fallback counts every gating check)", got)
	}
	if len(tr.Heads[1].RequiredRed) != 0 || len(tr.Heads[1].NonRequiredRed) != 0 || len(tr.Heads[1].MissingRequired) != 0 {
		t.Fatalf("fallback reading must not classify reds by required set: %+v", tr.Heads[1])
	}
	// c1 has no checks and is skipped; c2 is red, c3 green → two evaluated.
	if tr.Evaluated != 2 || tr.PassesToGreen != 2 || tr.ScanCapped {
		t.Fatalf("convergence = evaluated %d passes %d capped %v, want 2/2/false", tr.Evaluated, tr.PassesToGreen, tr.ScanCapped)
	}
}

// Every upstream failure surfaces as an error naming the PR or head, and a
// client without a GitHub transport refuses up front.
func TestPRCIRecoveryTrailErrors(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.PRCIRecoveryTrail(context.Background(), "o/r", 7, "c1"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client err = %v, want ErrNoGitHubClient", err)
	}

	base := func() *trailFixture {
		return &trailFixture{
			state:     "open",
			baseRef:   "v5",
			protected: true,
			required:  []string{"test"},
			commits:   []string{"c1", "c2"},
			checks: map[string][]trailCheck{
				"c1": {trailCheckRun("test", "completed", "failure")},
				"c2": {trailCheckRun("test", "completed", "success")},
			},
		}
	}
	cases := []struct {
		name string
		mut  func(*trailFixture)
		want string
	}{
		{"pr lookup fails", func(f *trailFixture) { f.prStatus = http.StatusNotFound }, "404"},
		{"commit list fails", func(f *trailFixture) { f.listStatus = http.StatusInternalServerError }, "listing commits for o/r#7"},
		{"head checks fail", func(f *trailFixture) { f.checksErr = "c2" }, "checks for o/r@c2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			tc.mut(f)
			srv := f.serve(t)
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"r"})
			_, err := c.PRCIRecoveryTrail(context.Background(), "o/r", 7, "c1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
