package automerge

// Tests for the fail-closed branches of the trusted-author sweep tier that the
// eligibility table does not reach: policy gates, API error paths, re-verify
// and merge-step failures, and the GitHub-signal helpers.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type trustedSweepFixture struct {
	author          string
	listStatus      int
	getStatus       int // first PR fetch
	recheckStatus   int // second PR fetch
	recheckHeadSHA  string
	headRepo        string
	mergeableState  string // default "clean"
	reviewsStatus   int
	permStatus      int
	permission      string
	mergeStatus     int
	mergeMessage    string
	mergeNotApplied bool
	commentStatus   int
	merges          int
	comments        int
	lastComment     string
	updates         int
	updateStatus    int
	labels          []string
	title           string
}

func newTrustedSweepAPI(t *testing.T, fx *trustedSweepFixture) *httptest.Server {
	t.Helper()
	fetches := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := fx.mergeableState
		if state == "" {
			state = "clean"
		}
		headRepo := fx.headRepo
		if headRepo == "" {
			headRepo = "acme/widget"
		}
		pull := func(sha string) map[string]any {
			author := fx.author
			if author == "" {
				author = "alice"
			}
			return map[string]any{
				"number":          7,
				"title":           fx.title,
				"state":           "open",
				"mergeable_state": state,
				"mergeable":       state == "clean",
				"user":            map[string]string{"login": author},
				"head":            map[string]any{"sha": sha, "repo": map[string]string{"full_name": headRepo}},
				"base":            map[string]any{"repo": map[string]string{"full_name": "acme/widget"}},
				"labels":          prLabelObjects(fx.labels),
			}
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			if fx.listStatus != 0 {
				http.Error(w, "list error", fx.listStatus)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{pull("sha7")})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			fetches++
			if fetches == 1 && fx.getStatus != 0 {
				http.Error(w, "get error", fx.getStatus)
				return
			}
			if fetches > 1 && fx.recheckStatus != 0 {
				http.Error(w, "recheck error", fx.recheckStatus)
				return
			}
			sha := "sha7"
			if fetches > 1 && fx.recheckHeadSHA != "" {
				sha = fx.recheckHeadSHA
			}
			json.NewEncoder(w).Encode(pull(sha))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			if fx.reviewsStatus != 0 {
				http.Error(w, "reviews error", fx.reviewsStatus)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/collaborators/alice/permission":
			if fx.permStatus != 0 {
				http.Error(w, "permission error", fx.permStatus)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"permission": fx.permission, "role_name": fx.permission})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/status":
			json.NewEncoder(w).Encode(map[string]any{
				"state":       "success",
				"total_count": 1,
				"statuses":    []map[string]string{{"context": "ci/build", "state": "success"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha7/check-runs":
			json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs":  []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
			fx.merges++
			if fx.mergeStatus != 0 {
				w.WriteHeader(fx.mergeStatus)
				msg := fx.mergeMessage
				if msg == "" {
					msg = "merge error"
				}
				json.NewEncoder(w).Encode(map[string]string{"message": msg})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"merged": !fx.mergeNotApplied, "sha": "merge7"})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
			fx.updates++
			if fx.updateStatus != 0 {
				w.WriteHeader(fx.updateStatus)
			}
			json.NewEncoder(w).Encode(map[string]any{"message": "Updating pull request branch."})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			fx.comments++
			var comment struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&comment)
			fx.lastComment = comment.Body
			if fx.commentStatus != 0 {
				http.Error(w, "comment error", fx.commentStatus)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
}

func newTrustedSweepEngine(apiURL string, policy TrustedAuthorPolicy, opts Options) *Engine {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	opts.TrustedAuthorPolicy = func() TrustedAuthorPolicy { return policy }
	opts.TrustedAuthorizer = func(login, requireRole string) TrustedAuthorDecision {
		return TrustedAuthorDecision{Allowed: login == "alice", Role: requireRole}
	}
	return New(client, opts)
}

func TestTrySweepTrustedAuthorPRFailClosedBranches(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fx          trustedSweepFixture
		policy      TrustedAuthorPolicy
		wantReason  string
		wantErr     bool
		wantMerges  int
		wantComment int
		wantUpdates int
	}{
		{name: "pr gone", fx: trustedSweepFixture{getStatus: http.StatusNotFound}, wantReason: "gone"},
		{name: "fetch error", fx: trustedSweepFixture{getStatus: http.StatusInternalServerError}, wantReason: "fetch-pr", wantErr: true},
		{name: "reviews error", fx: trustedSweepFixture{reviewsStatus: http.StatusInternalServerError}, wantReason: "review-state-check", wantErr: true},
		{name: "not mergeable", fx: trustedSweepFixture{mergeableState: "dirty"}, wantReason: "conflicting"},
		{
			name: "fork permission api error", fx: trustedSweepFixture{headRepo: "alice/widget", permStatus: http.StatusInternalServerError},
			wantReason: "author-permission-check", wantErr: true,
		},
		{
			name: "fork non-member", fx: trustedSweepFixture{headRepo: "alice/widget", permission: "read"},
			wantReason: "fork-non-member",
		},
		{
			name: "fork member merges", fx: trustedSweepFixture{headRepo: "alice/widget", permission: "write"},
			wantMerges: 1, wantComment: 1,
		},
		{
			name:       "required permission api error",
			fx:         trustedSweepFixture{permStatus: http.StatusInternalServerError},
			policy:     TrustedAuthorPolicy{RequireGitHubPermission: true},
			wantReason: "author-permission-check", wantErr: true,
		},
		{name: "recheck gone", fx: trustedSweepFixture{recheckStatus: http.StatusNotFound}, wantReason: "gone"},
		{name: "recheck error", fx: trustedSweepFixture{recheckStatus: http.StatusInternalServerError}, wantReason: "fetch-pr-recheck", wantErr: true},
		{name: "head changed since eval", fx: trustedSweepFixture{recheckHeadSHA: "sha8"}, wantReason: "head-changed-since-eval"},
		{
			name:        "behind updates branch",
			fx:          trustedSweepFixture{mergeableState: "behind"},
			wantReason:  "check-pending",
			wantUpdates: 1,
		},
		{
			// hivecommons/hive#15103: prow's needs-rebase label lags; when
			// GitHub says "behind" the label must not stop the UpdateBranch
			// that clears it.
			name:        "stale needs-rebase label with behind still updates branch",
			fx:          trustedSweepFixture{mergeableState: "behind", labels: []string{"needs-rebase"}},
			wantReason:  "check-pending",
			wantUpdates: 1,
		},
		{
			name:       "needs-rebase label with dirty is left to the fixer",
			fx:         trustedSweepFixture{mergeableState: "dirty", labels: []string{"needs-rebase"}},
			wantReason: "label:needs-rebase",
		},
		{
			name:        "required checks expected updates branch",
			fx:          trustedSweepFixture{mergeStatus: http.StatusMethodNotAllowed, mergeMessage: `2 of 3 required status checks are expected.`},
			wantReason:  "check-pending",
			wantMerges:  1,
			wantUpdates: 1,
		},
		{
			name:        "required checks expected update conflict",
			fx:          trustedSweepFixture{mergeStatus: http.StatusMethodNotAllowed, mergeMessage: `Required status check "changelog-fragment-guard" is expected.`, updateStatus: http.StatusUnprocessableEntity},
			wantReason:  "conflicting",
			wantMerges:  1,
			wantUpdates: 1,
		},
		{name: "merge api error", fx: trustedSweepFixture{mergeStatus: http.StatusConflict}, wantReason: "merge-failed", wantErr: true, wantMerges: 1},
		{
			name:       "merge queue required is a skip not an error",
			fx:         trustedSweepFixture{mergeStatus: http.StatusMethodNotAllowed, mergeMessage: mergeQueueRequiredTestMessage},
			wantReason: autoMergeReasonMergeQueueRequired,
			wantMerges: 1,
		},
		{name: "merge not applied", fx: trustedSweepFixture{mergeNotApplied: true}, wantReason: "merge-not-applied", wantMerges: 1},
		{name: "comment failure does not undo merge", fx: trustedSweepFixture{commentStatus: http.StatusInternalServerError}, wantMerges: 1, wantComment: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fx
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			policy := tc.policy
			policy.Enabled = true
			policy.RequireRole = "merger"
			engine := newTrustedSweepEngine(api.URL, policy, Options{})
			event, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fx.merges != tc.wantMerges || fx.comments != tc.wantComment || fx.updates != tc.wantUpdates {
				t.Fatalf("merges=%d comments=%d updates=%d, want %d/%d/%d", fx.merges, fx.comments, fx.updates, tc.wantMerges, tc.wantComment, tc.wantUpdates)
			}
			if tc.wantReason == "" && (event.Tier != "trusted-author" || event.Number != 7 || event.MergeSHA != "merge7") {
				t.Fatalf("event = %+v, want trusted-author merge of PR 7", event)
			}
		})
	}
}

func TestSweepTrustedAuthorAutoMergesPolicyGates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy      TrustedAuthorPolicy
		opts        Options
		fx          trustedSweepFixture
		wantErr     bool
		wantSeen    int
		wantSkipped int
		wantMerged  int
	}{
		{name: "tier disabled", policy: TrustedAuthorPolicy{}},
		{name: "repo outside allow-list", policy: TrustedAuthorPolicy{Enabled: true, Repos: map[string]bool{"other": true}}},
		{
			name:   "repo auto-merge disabled",
			policy: TrustedAuthorPolicy{Enabled: true},
			opts:   Options{RepoAutoMergeEnabled: func(string) bool { return false }},
		},
		{name: "list error is returned", policy: TrustedAuthorPolicy{Enabled: true}, fx: trustedSweepFixture{listStatus: http.StatusInternalServerError}, wantErr: true},
		{
			name: "per-PR error is skipped not fatal", policy: TrustedAuthorPolicy{Enabled: true},
			fx: trustedSweepFixture{reviewsStatus: http.StatusInternalServerError}, wantSeen: 1, wantSkipped: 1,
		},
		{
			name: "not mergeable is skipped", policy: TrustedAuthorPolicy{Enabled: true},
			fx: trustedSweepFixture{mergeableState: "dirty"}, wantSeen: 1, wantSkipped: 1,
		},
		{
			name: "allow-listed repo merges", policy: TrustedAuthorPolicy{Enabled: true, Repos: map[string]bool{" Widget ": false, "widget": true}},
			wantSeen: 1, wantMerged: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fx
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			engine := newTrustedSweepEngine(api.URL, tc.policy, tc.opts)
			var audits []AutoMergeSweepEvent
			result, err := engine.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{
				Audit: func(event AutoMergeSweepEvent) { audits = append(audits, event) },
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if result.Seen != tc.wantSeen || result.Skipped != tc.wantSkipped || len(result.Merged) != tc.wantMerged || len(audits) != tc.wantMerged {
				t.Fatalf("result = %+v audits=%d, want seen=%d skipped=%d merged=%d", result, len(audits), tc.wantSeen, tc.wantSkipped, tc.wantMerged)
			}
		})
	}
}

func TestSweepTrustedAuthorAutoMergesWrapperAndNilEngine(t *testing.T) {
	if _, err := SweepTrustedAuthorAutoMerges(context.Background(), nil, Options{}, AutoMergeSweepOptions{}); err != hgithub.ErrNoGitHubClient {
		t.Fatalf("wrapper error = %v, want hgithub.ErrNoGitHubClient", err)
	}
	var nilEngine *Engine
	if _, err := nilEngine.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != hgithub.ErrNoGitHubClient {
		t.Fatalf("nil engine error = %v, want hgithub.ErrNoGitHubClient", err)
	}
	if policy := nilEngine.currentTrustedAuthorPolicy(); policy.Enabled {
		t.Fatalf("nil engine policy = %+v, want disabled", policy)
	}
	if decision := nilEngine.trustedAuthorDecision("alice", "merger"); decision.Allowed {
		t.Fatalf("nil engine decision = %+v, want denied", decision)
	}
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "")
	noAuthorizer := New(client, Options{})
	if decision := noAuthorizer.trustedAuthorDecision("alice", "merger"); decision.Allowed {
		t.Fatalf("missing authorizer decision = %+v, want fail closed", decision)
	}
	if policy := noAuthorizer.currentTrustedAuthorPolicy(); policy.Enabled {
		t.Fatalf("missing policy func = %+v, want disabled", policy)
	}
}

func TestTrustedAuthorPolicyRepoAndLabelEdges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy TrustedAuthorPolicy
		repo   string
		want   bool
	}{
		{name: "disabled", policy: TrustedAuthorPolicy{Repos: map[string]bool{"widget": true}}, repo: "widget"},
		{name: "empty allow-list admits all", policy: TrustedAuthorPolicy{Enabled: true}, repo: "anything", want: true},
		{name: "case and space insensitive", policy: TrustedAuthorPolicy{Enabled: true, Repos: map[string]bool{"widget": true}}, repo: " Widget ", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.repoAllowed(tc.repo); got != tc.want {
				t.Fatalf("repoAllowed(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
	policy := TrustedAuthorPolicy{ExcludeLabels: map[string]bool{"needs-human": true}}
	if got := policy.excludedLabel([]string{"bug", "feature"}); got != "" {
		t.Fatalf("excludedLabel = %q, want none", got)
	}
}

func TestPrefilterTrustedAuthorPRHeldLabel(t *testing.T) {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "")
	engine := New(client, Options{})
	policy := TrustedAuthorPolicy{RequireRole: "merger"}
	pr := &gh.PullRequest{
		State:  gh.Ptr("open"),
		User:   &gh.User{Login: gh.Ptr("alice")},
		Head:   &gh.PullRequestBranch{SHA: gh.Ptr("sha")},
		Labels: []*gh.Label{{Name: gh.Ptr("hold")}},
	}
	if got := engine.prefilterTrustedAuthorPR("acme", "widget", pr, policy); got != "label:hold" {
		t.Fatalf("prefilterTrustedAuthorPR = %q, want label:hold", got)
	}
	pr.Labels = nil
	if got := engine.prefilterTrustedAuthorPR("acme", "widget", pr, policy); got != "untrusted-author-role" {
		t.Fatalf("prefilterTrustedAuthorPR without authorizer = %q, want untrusted-author-role", got)
	}
}

func TestPrefilterTrustedAuthorPRIgnoresSentinelExcludedLabel(t *testing.T) {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "")
	engine := New(client, Options{
		SentinelLabel: func() string { return "sentinel-alert" },
		TrustedAuthorizer: func(login, requireRole string) TrustedAuthorDecision {
			return TrustedAuthorDecision{Allowed: true, Role: requireRole}
		},
	})
	policy := TrustedAuthorPolicy{
		RequireRole:   "merger",
		ExcludeLabels: map[string]bool{"sentinel-alert": true},
	}
	pr := &gh.PullRequest{
		State:  gh.Ptr("open"),
		User:   &gh.User{Login: gh.Ptr("alice")},
		Head:   &gh.PullRequestBranch{SHA: gh.Ptr("sha")},
		Labels: []*gh.Label{{Name: gh.Ptr("sentinel-alert")}},
	}
	if got := engine.prefilterTrustedAuthorPR("acme", "widget", pr, policy); got != "" {
		t.Fatalf("prefilterTrustedAuthorPR = %q, want eligible", got)
	}
}

func TestTrustedAuthorGitHubSignalErrorsAndEdges(t *testing.T) {
	page := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/widget/collaborators/ghost/permission":
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.URL.Path == "/repos/acme/widget/collaborators/maint/permission":
			json.NewEncoder(w).Encode(map[string]string{"permission": "read", "role_name": " Maintain "})
		case r.URL.Path == "/repos/acme/widget/collaborators/plain/permission":
			json.NewEncoder(w).Encode(map[string]string{"permission": "Admin"})
		case r.URL.Path == "/repos/acme/widget/pulls/9/reviews":
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.URL.Path == "/repos/acme/widget/pulls/10/reviews":
			page++
			if r.URL.Query().Get("page") == "" || r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/acme/widget/pulls/10/reviews?page=2>; rel="next"`)
				json.NewEncoder(w).Encode([]map[string]any{
					{"state": "COMMENTED", "user": map[string]string{"login": "reviewer"}},
					{"state": "CHANGES_REQUESTED", "user": map[string]string{"login": "reviewer"}},
					{"state": "CHANGES_REQUESTED"},
				})
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{
				{"state": "DISMISSED", "user": map[string]string{"login": "reviewer"}},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer api.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	engine := New(client, Options{})

	if _, err := engine.authorRepoPermission(context.Background(), "acme", "widget", "ghost"); err == nil {
		t.Fatal("authorRepoPermission should surface API errors")
	}
	if p, err := engine.authorRepoPermission(context.Background(), "acme", "widget", "maint"); err != nil || !p.Allowed || p.Level != "maintain" {
		t.Fatalf("maint permission = %+v err=%v, want allowed maintain via role name", p, err)
	}
	if p, err := engine.authorRepoPermission(context.Background(), "acme", "widget", "plain"); err != nil || !p.Allowed || p.Level != "admin" {
		t.Fatalf("plain permission = %+v err=%v, want allowed admin via permission", p, err)
	}
	if _, err := engine.hasOutstandingChangesRequested(context.Background(), "acme", "widget", 9); err == nil {
		t.Fatal("hasOutstandingChangesRequested should surface API errors")
	}
	if blocked, err := engine.hasOutstandingChangesRequested(context.Background(), "acme", "widget", 10); err != nil || blocked {
		t.Fatalf("PR 10 blocked=%v err=%v, want dismissed on page 2 to clear the block", blocked, err)
	}
	if page != 2 {
		t.Fatalf("review pages fetched = %d, want 2", page)
	}

	repo := func(name string) *gh.Repository { return &gh.Repository{FullName: gh.Ptr(name)} }
	for _, tc := range []struct {
		name string
		pr   *gh.PullRequest
		want bool
	}{
		{name: "nil", pr: nil},
		{name: "missing head", pr: &gh.PullRequest{Base: &gh.PullRequestBranch{Repo: repo("acme/widget")}}},
		{name: "missing base", pr: &gh.PullRequest{Head: &gh.PullRequestBranch{Repo: repo("acme/widget")}}},
		{name: "missing head repo", pr: &gh.PullRequest{Head: &gh.PullRequestBranch{}, Base: &gh.PullRequestBranch{Repo: repo("acme/widget")}}},
		{name: "missing base repo", pr: &gh.PullRequest{Head: &gh.PullRequestBranch{Repo: repo("acme/widget")}, Base: &gh.PullRequestBranch{}}},
		{name: "same repo different case", pr: &gh.PullRequest{Head: &gh.PullRequestBranch{Repo: repo("ACME/widget")}, Base: &gh.PullRequestBranch{Repo: repo("acme/widget")}}},
		{name: "cross repo", pr: &gh.PullRequest{Head: &gh.PullRequestBranch{Repo: repo("alice/widget")}, Base: &gh.PullRequestBranch{Repo: repo("acme/widget")}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := engine.isForkPR(tc.pr); got != tc.want {
				t.Fatalf("isForkPR = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTrustedAuthorSweepRunsReviewGate pins hivecommons/hive#12974: the swarm
// reviews every PR, so an enforcing review gate holds a trusted human's PR
// until the swarm approves its head, exactly as it holds the App's own.
// #12966 merged through this tier with zero swarm reviews because the gate
// covered the self-authored lane only. Shadow mode records and merges as
// before; a swarm approve merges in every mode.
func TestTrustedAuthorSweepRunsReviewGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		verdict    ReviewGateVerdict
		wantReason string
		wantMerges int
	}{
		{name: "enforce holds unreviewed head", mode: ReviewGateModeEnforce, verdict: reviewGateNoBundle, wantReason: autoMergeReasonReviewGatePrefix + ReviewGateOutcomeNoBundle},
		{name: "enforce holds changes requested", mode: ReviewGateModeEnforce, verdict: reviewGateChanges, wantReason: autoMergeReasonReviewGatePrefix + "changes-requested:correctness,security"},
		{name: "enforce merges approved head", mode: ReviewGateModeEnforce, verdict: reviewGateApproved, wantMerges: 1},
		{name: "shadow records and merges", mode: ReviewGateModeShadow, verdict: reviewGateNoBundle, wantMerges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := trustedSweepFixture{}
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
			engine := newTrustedSweepEngine(api.URL, policy, Options{})
			var rec reviewGateRecords
			asked := 0
			engine.SetReviewGate(&ReviewGate{
				Mode:             func() string { return tc.mode },
				Verdict:          func(repo string, number int, head string) ReviewGateVerdict { asked++; return tc.verdict },
				RecordEvaluation: func(e ReviewGateEvaluation) { rec.evals = append(rec.evals, e) },
			})
			_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fx.merges != tc.wantMerges {
				t.Fatalf("merges = %d, want %d", fx.merges, tc.wantMerges)
			}
			if asked != 1 {
				t.Fatalf("swarm verdict asked %d times, want once on the re-verified head", asked)
			}
			if len(rec.evals) != 1 || rec.evals[0].Lane != stageLaneTrusted || rec.evals[0].HeadSHA != "sha7" {
				t.Fatalf("evaluations = %+v, want one on lane %q head sha7", rec.evals, stageLaneTrusted)
			}
		})
	}
}

func prLabelObjects(names []string) []map[string]string {
	out := make([]map[string]string, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	return out
}

// Regression for "Approve and let hive merge" on an untrusted author's PR:
// the operator's approval admits the head on the stage record, and the
// trusted-author tier must honour it instead of parking the PR at
// untrusted-author-role forever. The merge is attributed to the operator,
// an admission for another head does not count, and a plain untrusted
// author is still refused.
func TestTrySweepTrustedAuthorPROperatorApprovalAdmitsUntrustedAuthor(t *testing.T) {
	for _, tc := range []struct {
		name        string
		admitHead   string
		permission  string
		policy      TrustedAuthorPolicy
		wantReason  string
		wantMerges  int
		wantComment string
	}{
		{name: "no approval", wantReason: "untrusted-author-role"},
		{name: "approval for another head", admitHead: "sha-old", wantReason: "untrusted-author-role"},
		{name: "approval admits head", admitHead: "sha7", wantMerges: 1, wantComment: "on behalf of @clubanderson"},
		{
			name: "approval stands in for repo permission", admitHead: "sha7", permission: "read",
			policy: TrustedAuthorPolicy{RequireGitHubPermission: true}, wantMerges: 1, wantComment: stagelabels.OperatorApprovalAdmission("clubanderson"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := trustedSweepFixture{author: "mallory", permission: tc.permission}
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			policy := tc.policy
			policy.Enabled = true
			policy.RequireRole = "merger"
			store := stagelabels.NewStore("", nil)
			key := stageKey("acme", "widget", 7)
			store.Observe(key, stagelabels.ReadyForHumanMerge, "untrusted-author-role")
			if tc.admitHead != "" && !store.NoteAdmission(key, tc.admitHead, stagelabels.OperatorApprovalAdmission("clubanderson"), time.Date(2026, 10, 15, 13, 1, 0, 0, time.UTC)) {
				t.Fatal("NoteAdmission refused the stage record")
			}
			engine := newTrustedSweepEngine(api.URL, policy, Options{StageStore: store})
			event, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.wantReason || fx.merges != tc.wantMerges {
				t.Fatalf("reason=%q merges=%d, want %q/%d", reason, fx.merges, tc.wantReason, tc.wantMerges)
			}
			if tc.wantMerges == 1 {
				if event.Author != "mallory" || event.Tier != "trusted-author" {
					t.Fatalf("event = %+v, want trusted-author merge of mallory's PR", event)
				}
				if !strings.Contains(fx.lastComment, tc.wantComment) || strings.Contains(fx.lastComment, "holds role") {
					t.Fatalf("merge comment must attribute the operator, not the author's role: %q", fx.lastComment)
				}
			}
		})
	}
}
