package github

// Tests for the rest of the bundle: the CI summary and human actions captured
// when a PR merges (review_evidence_merge.go), and the sentinel findings the
// sweep appends to the flagged head's bundle (review_evidence.go). Every
// capture is best-effort: a GitHub failure leaves that part empty and never
// stops the merge from being recorded.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/sentinel"
)

type evidenceContextFixture struct {
	mu          sync.Mutex
	head        string
	checksFail  bool
	reviewsFail bool
	eventsFail  bool
	checkRuns   []map[string]any
	reviews     []map[string]any
	events      []map[string]any
	checkHits   int
}

func newEvidenceContextServer(t *testing.T, f *evidenceContextFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fail := func(failed bool) bool {
			if failed {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return failed
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "head": map[string]any{"sha": f.head},
				"merged_by": map[string]any{"login": "hive-app[bot]"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/commits/"+f.head+"/check-runs":
			f.checkHits++
			if !fail(f.checksFail) {
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(f.checkRuns), "check_runs": f.checkRuns})
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			if !fail(f.reviewsFail) {
				_ = json.NewEncoder(w).Encode(f.reviews)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/events":
			if !fail(f.eventsFail) {
				_ = json.NewEncoder(w).Encode(f.events)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRecordReviewEvidenceMergeCapturesCIAndHumanActions(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	checkRuns := []map[string]any{
		{"id": 1, "name": "test", "status": "completed", "conclusion": "success", "html_url": "https://ci/test", "app": map[string]any{"id": 1}},
		{"id": 2, "name": "build", "status": "completed", "conclusion": "failure", "html_url": "https://ci/build", "app": map[string]any{"id": 1}},
		{"id": 3, "name": "lint", "status": "in_progress", "app": map[string]any{"id": 1}},
		{"id": 4, "name": "", "status": "completed", "conclusion": "success"},
	}
	wantCI := []evidence.Check{
		{Name: "build", Conclusion: "failure", URL: "https://ci/build"},
		{Name: "lint", Conclusion: "in_progress"},
		{Name: "test", Conclusion: "success", URL: "https://ci/test"},
	}
	human := map[string]any{"login": "bob", "type": "User"}
	bot := map[string]any{"login": "hive-app[bot]", "type": "Bot"}
	reviews := []map[string]any{
		// Approval of an older head, before the bundle was started: not this head's evidence.
		{"id": 1, "user": human, "state": "APPROVED", "commit_id": "h0", "submitted_at": "2026-09-30T00:00:00Z", "html_url": "https://r/1"},
		// Approval of this head that predates the bundle still counts.
		{"id": 2, "user": human, "state": "APPROVED", "commit_id": "h1", "submitted_at": "2026-09-30T12:00:00Z", "html_url": "https://r/2"},
		{"id": 3, "user": bot, "state": "APPROVED", "commit_id": "h1", "submitted_at": "2026-10-01T01:00:00Z"},
		{"id": 4, "user": human, "state": "COMMENTED", "commit_id": "h1", "submitted_at": "2026-10-01T01:00:00Z"},
	}
	events := []map[string]any{
		{"event": "labeled", "actor": human, "label": map[string]any{"name": "lgtm"}, "created_at": "2026-10-01T02:00:00Z"},
		{"event": "unlabeled", "actor": human, "label": map[string]any{"name": "hold"}, "created_at": "2026-10-01T03:00:00Z"},
		{"event": "unlabeled", "actor": human, "label": map[string]any{"name": "triage"}, "created_at": "2026-10-01T04:00:00Z"},
		{"event": "labeled", "actor": bot, "label": map[string]any{"name": "hive/queued"}, "created_at": "2026-10-01T02:30:00Z"},
		{"event": "labeled", "actor": human, "label": map[string]any{"name": "early"}, "created_at": "2026-09-30T00:00:00Z"},
		{"event": "closed", "actor": human, "created_at": "2026-10-01T05:00:00Z"},
	}
	approval := evidence.Action{Actor: "bob", Kind: ReviewEvidenceActionApproval, Detail: "https://r/2", At: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	labelActions := []evidence.Action{
		{Actor: "bob", Kind: ReviewEvidenceActionLabelAdded, Detail: "lgtm", At: t0.Add(2 * time.Hour)},
		{Actor: "bob", Kind: ReviewEvidenceActionHoldLift, Detail: "hold", At: t0.Add(3 * time.Hour)},
		{Actor: "bob", Kind: ReviewEvidenceActionLabelRemoved, Detail: "triage", At: t0.Add(4 * time.Hour)},
	}

	tests := []struct {
		name        string
		fixture     *evidenceContextFixture
		wantCI      []evidence.Check
		wantActions []evidence.Action
	}{
		{
			name:        "CI and human actions captured",
			fixture:     &evidenceContextFixture{head: "h1", checkRuns: checkRuns, reviews: reviews, events: events},
			wantCI:      wantCI,
			wantActions: append([]evidence.Action{approval}, labelActions...),
		},
		{
			name:        "check runs fail leaves CI empty",
			fixture:     &evidenceContextFixture{head: "h1", checksFail: true, reviews: reviews, events: events},
			wantActions: append([]evidence.Action{approval}, labelActions...),
		},
		{
			name:    "reviews fail leaves human actions empty",
			fixture: &evidenceContextFixture{head: "h1", checkRuns: checkRuns, reviewsFail: true, events: events},
			wantCI:  wantCI,
		},
		{
			name:    "events fail leaves human actions empty",
			fixture: &evidenceContextFixture{head: "h1", checkRuns: checkRuns, reviews: reviews, eventsFail: true},
			wantCI:  wantCI,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withEvidenceRoot(t)
			writeTestEvidence(t, root, "h1", "", t0)
			f := tt.fixture
			c := newTestClient(t, newEvidenceContextServer(t, f), "o", []string{"o/r"})
			c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })

			c.RecordPRMergedAudit("o/r", 7, "squash", "m1", PRAuditPathSweep)

			b := mustLoadBundle(t, root, "h1")
			if b.Merge == nil || b.Merge.SHA != "m1" {
				t.Fatalf("merge not recorded: %+v", b.Merge)
			}
			if !reflect.DeepEqual(b.CI.Checks, tt.wantCI) {
				t.Fatalf("ci checks = %+v, want %+v", b.CI.Checks, tt.wantCI)
			}
			if (len(tt.wantCI) > 0) == b.CI.CapturedAt.IsZero() {
				t.Fatalf("ci captured_at = %v with %d checks", b.CI.CapturedAt, len(tt.wantCI))
			}
			if !reflect.DeepEqual(b.HumanActions, tt.wantActions) {
				t.Fatalf("human actions = %+v, want %+v", b.HumanActions, tt.wantActions)
			}
			if err := b.Validate(); err != nil {
				t.Fatalf("bundle invalid: %v", err)
			}
			if h, _ := evidence.Hash(b); h != b.Hash {
				t.Fatalf("hash not resealed: %s vs %s", h, b.Hash)
			}

			// A replayed merge neither refetches CI nor changes the bundle.
			hits := f.checkHits
			c.RecordPRMergedAudit("o/r", 7, "squash", "m1", PRAuditPathRelay)
			if f.checkHits != hits {
				t.Fatalf("replayed merge refetched check runs")
			}
			if again := mustLoadBundle(t, root, "h1"); again.Hash != b.Hash {
				t.Fatalf("replayed merge changed the bundle")
			}
		})
	}
}

func TestEvidenceHumanUser(t *testing.T) {
	tests := []struct {
		name string
		user *gh.User
		want bool
	}{
		{name: "nil", want: false},
		{name: "person", user: &gh.User{Login: gh.Ptr("bob"), Type: gh.Ptr("User")}, want: true},
		{name: "bot type", user: &gh.User{Login: gh.Ptr("ci"), Type: gh.Ptr("Bot")}, want: false},
		{name: "bot suffix", user: &gh.User{Login: gh.Ptr("dependabot[bot]")}, want: false},
		{name: "empty login", user: &gh.User{Type: gh.Ptr("User")}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evidenceHumanUser(tt.user); got != tt.want {
				t.Fatalf("evidenceHumanUser = %v, want %v", got, tt.want)
			}
		})
	}
}

func evidenceSentinelPR(head string) *gh.PullRequest {
	return &gh.PullRequest{
		Number: gh.Ptr(7),
		User:   &gh.User{Login: gh.Ptr("alice"), Type: gh.Ptr("User")},
		Base:   &gh.PullRequestBranch{SHA: gh.Ptr("base000")},
		Head:   &gh.PullRequestBranch{SHA: gh.Ptr(head)},
	}
}

func TestRecordReviewEvidenceSentinel(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := t0.Add(time.Hour)
	findings := []sentinel.Finding{
		{Rule: sentinel.RuleSensitivePath, Summary: "touches OWNERS", Paths: []string{"z.yml", " OWNERS "}},
		{Rule: "", Summary: "no rule, dropped"},
	}
	want := []evidence.SentinelFinding{{Rule: sentinel.RuleSensitivePath, Summary: "touches OWNERS", Paths: []string{"OWNERS", "z.yml"}}}

	tests := []struct {
		name         string
		settings     *ReviewEvidenceSettings
		seed         bool
		corrupt      bool
		pr           *gh.PullRequest
		findings     []sentinel.Finding
		wantSentinel []evidence.SentinelFinding
		wantNoBundle bool
		wantVerdicts int
	}{
		{name: "not wired", pr: evidenceSentinelPR("h1"), findings: findings, wantNoBundle: true},
		{name: "disabled", settings: &ReviewEvidenceSettings{}, pr: evidenceSentinelPR("h1"), findings: findings, wantNoBundle: true},
		{name: "no findings", settings: &ReviewEvidenceSettings{Enabled: true}, pr: evidenceSentinelPR("h1"), wantNoBundle: true},
		{name: "only ruleless findings", settings: &ReviewEvidenceSettings{Enabled: true}, pr: evidenceSentinelPR("h1"), findings: findings[1:], wantNoBundle: true},
		{name: "no head", settings: &ReviewEvidenceSettings{Enabled: true}, pr: evidenceSentinelPR(""), findings: findings, wantNoBundle: true},
		{
			name: "appends to existing bundle", settings: &ReviewEvidenceSettings{Enabled: true}, seed: true,
			pr: evidenceSentinelPR("h1"), findings: findings, wantSentinel: want, wantVerdicts: 1,
		},
		{
			name: "starts a bundle from the listed PR", settings: &ReviewEvidenceSettings{Enabled: true},
			pr: evidenceSentinelPR("h1"), findings: findings, wantSentinel: want,
		},
		{
			name: "unreadable bundle left alone", settings: &ReviewEvidenceSettings{Enabled: true}, corrupt: true,
			pr: evidenceSentinelPR("h1"), findings: findings,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withEvidenceRoot(t)
			if tt.seed {
				writeTestEvidence(t, root, "h1", "", t0)
			}
			path := ReviewEvidencePath(root, "o/r", 7, "h1")
			if tt.corrupt {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			c := evidenceTestClient(t, &evidencePRFixture{head: "h1"}, tt.settings)

			c.recordReviewEvidenceSentinel("o/r", tt.pr, tt.findings, now)

			if tt.wantNoBundle {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("bundle written: %v", err)
				}
				return
			}
			if tt.corrupt {
				if data, _ := os.ReadFile(path); string(data) != "{broken" {
					t.Fatalf("unreadable bundle overwritten: %s", data)
				}
				return
			}
			b := mustLoadBundle(t, root, "h1")
			if !reflect.DeepEqual(b.Sentinel, tt.wantSentinel) {
				t.Fatalf("sentinel = %+v, want %+v", b.Sentinel, tt.wantSentinel)
			}
			if len(b.Verdicts) != tt.wantVerdicts {
				t.Fatalf("verdicts = %d, want %d", len(b.Verdicts), tt.wantVerdicts)
			}
			if b.BaseSHA != "base000" || b.Author.Login != "alice" || !b.UpdatedAt.Equal(now) {
				t.Fatalf("bundle metadata = base %q author %+v updated %v", b.BaseSHA, b.Author, b.UpdatedAt)
			}
			if err := b.Validate(); err != nil {
				t.Fatalf("bundle invalid: %v", err)
			}

			// The same finding flagged again is not duplicated and the file is untouched.
			c.recordReviewEvidenceSentinel("o/r", tt.pr, tt.findings, now.Add(time.Hour))
			if again := mustLoadBundle(t, root, "h1"); again.Hash != b.Hash || len(again.Sentinel) != len(tt.wantSentinel) {
				t.Fatalf("re-flag changed the bundle: %+v", again.Sentinel)
			}
		})
	}
}

func TestSweepSentinelRecordsReviewEvidence(t *testing.T) {
	root := withEvidenceRoot(t)
	pr := sentinelPR(7, "vjymisal0", "aaa111")
	pr["base"] = map[string]string{"sha": "base000"}
	f := &sentinelFixture{
		prs: []map[string]any{pr},
		files: map[int][]map[string]any{7: {{
			"filename": "OWNERS", "status": "modified", "additions": 1, "deletions": 0,
			"patch": "@@ -5,3 +5,4 @@\n reviewers:\n   - clubanderson\n+  - vjymisal0\n",
		}}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })

	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 1 {
		t.Fatalf("sweep: res=%+v err=%v", res, err)
	}
	b := mustLoadBundle(t, root, "aaa111")
	var rules []string
	for _, s := range b.Sentinel {
		rules = append(rules, s.Rule)
	}
	if want := res.Flagged[0].Rules(); !reflect.DeepEqual(rules, want) {
		t.Fatalf("bundle sentinel rules = %v, want %v", rules, want)
	}
	if b.Author.Login != "vjymisal0" || b.BaseSHA != "base000" {
		t.Fatalf("bundle started with author %+v base %q", b.Author, b.BaseSHA)
	}
}
