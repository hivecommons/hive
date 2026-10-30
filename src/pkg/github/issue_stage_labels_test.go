package github

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// issueStageServer is a fake GitHub that records label writes and comments
// and answers the label-family provisioning list with everything present.
type issueStageServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	added    map[int][]string
	removed  map[int][]string
	comments map[int]int
	lists    int
	// truth is what GET /issues/N reports — the fresh read Enforce takes
	// before writing. Tests seed it from the enumeration payload (seed) or
	// set it to a different label set to simulate another writer.
	truth map[int][]string
	gets  map[int]int
	// prs marks truth numbers the list-by-label call reports as pull
	// requests (GitHub's issues feed returns PRs too).
	prs map[int]bool
	// deletedDefs records repo label definitions deleted via
	// DELETE /repos/acme/widgets/labels/<name> (hivecommons/hive#12683).
	deletedDefs []string
	// subIssues is what GET /issues/N/sub_issues reports per parent;
	// closers is the merged PR GraphQL names for a closed child; closed
	// records PATCH state=closed per issue with its state_reason
	// (hivecommons/hive#12857).
	subIssues map[int][]subIssueRef
	closers   map[int]int
	closed    map[int]string
	bodies    map[int]string
	prBodies  map[int]string
	prBases   map[int]string
	prAuthors map[int]string
	prPatches map[int]string
	// commentBodies keeps every comment body posted per issue.
	commentBodies map[int][]string
	// closeFails makes PATCH state=closed answer 502 for the issue so a
	// close that fails after its comment landed can be replayed.
	closeFails map[int]bool
	// prRateLimited answers PR link lookups with a secondary rate limit and
	// counts them in prLookups (guarded by mu).
	prRateLimited bool
	prLookups     int
}

func newIssueStageServer(t *testing.T) *issueStageServer {
	t.Helper()
	s := &issueStageServer{added: map[int][]string{}, removed: map[int][]string{}, comments: map[int]int{}, truth: map[int][]string{}, gets: map[int]int{}, prs: map[int]bool{}, subIssues: map[int][]subIssueRef{}, closers: map[int]int{}, closed: map[int]string{}, bodies: map[int]string{}, prBodies: map[int]string{}, prBases: map[int]string{}, prAuthors: map[int]string{}, prPatches: map[int]string{}, commentBodies: map[int][]string{}, closeFails: map[int]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path, _ := url.PathUnescape(r.URL.Path)
		if r.Method == http.MethodPost && path == "/graphql" {
			var q struct {
				Query     string `json:"query"`
				Variables struct {
					Issue int `json:"issue"`
					PR    int `json:"pr"`
				} `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&q)
			if strings.Contains(q.Query, "pullRequest(number:$pr)") {
				s.mu.Lock()
				s.prLookups++
				limited := s.prRateLimited
				s.mu.Unlock()
				if limited {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
					return
				}
				pr := q.Variables.PR
				base := s.prBases[pr]
				if base == "" {
					base = "v5"
				}
				author := s.prAuthors[pr]
				if author == "" {
					author = "hive[bot]"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
					"defaultBranchRef": map[string]any{"name": "v5"},
					"pullRequest": map[string]any{
						"state":       "OPEN",
						"baseRefName": base,
						"body":        s.prBodies[pr],
						"url":         "https://github.com/acme/widgets/pull/" + strconv.Itoa(pr),
						"author":      map[string]any{"login": author},
					},
				}}})
				return
			}
			s.mu.Lock()
			pr := s.closers[q.Variables.Issue]
			s.mu.Unlock()
			nodes := []map[string]any{}
			if pr > 0 {
				nodes = append(nodes, map[string]any{"number": pr, "url": "https://github.com/acme/widgets/pull/" + strconv.Itoa(pr), "merged": true})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"closedByPullRequestsReferences": map[string]any{"nodes": nodes}}}}})
			return
		}
		const prefix = "/repos/acme/widgets/"
		if !strings.HasPrefix(path, prefix) {
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rest := strings.TrimPrefix(path, prefix)
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(rest, "pulls/"):
			pr, _ := strconv.Atoi(strings.TrimPrefix(rest, "pulls/"))
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.prPatches[pr] = body.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"number": pr, "body": body.Body})
		case r.Method == http.MethodGet && rest == "issues":
			// List-by-label used by the legacy decision-label retirement:
			// answer from truth so carriers block definition deletion.
			want := strings.ToLower(r.URL.Query().Get("labels"))
			out := []map[string]any{}
			for n, labels := range s.truth {
				for _, l := range labels {
					if strings.ToLower(l) == want {
						item := map[string]any{"number": n}
						if s.prs[n] {
							item["pull_request"] = map[string]any{"url": "https://api.example/pulls"}
						}
						out = append(out, item)
						break
					}
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodDelete && strings.HasPrefix(rest, "labels/"):
			s.deletedDefs = append(s.deletedDefs, strings.TrimPrefix(rest, "labels/"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && rest == "labels":
			s.lists++
			var out []map[string]string
			for _, d := range stagelabels.AllLabelDefs() {
				out = append(out, map[string]string{"name": d.Name, "color": d.Color})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasPrefix(rest, "issues/") && !strings.Contains(strings.TrimPrefix(rest, "issues/"), "/"):
			var n int
			_, _ = sscanIssue(rest, &n)
			s.gets[n]++
			labels := make([]map[string]string, 0, len(s.truth[n]))
			for _, l := range s.truth[n] {
				labels = append(labels, map[string]string{"name": l})
			}
			state := "open"
			if _, done := s.closed[n]; done {
				state = "closed"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "labels": labels, "state": state, "body": s.bodies[n], "user": map[string]any{"login": "hive[bot]", "type": "Bot"}})
		case r.Method == http.MethodGet && strings.HasSuffix(rest, "/comments"):
			var n int
			_, _ = sscanIssue(rest, &n)
			out := []map[string]string{}
			for _, body := range s.commentBodies[n] {
				out = append(out, map[string]string{"body": body})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasSuffix(rest, "/sub_issues"):
			var n int
			_, _ = sscanIssue(rest, &n)
			refs := s.subIssues[n]
			if refs == nil {
				refs = []subIssueRef{}
			}
			_ = json.NewEncoder(w).Encode(refs)
		case r.Method == http.MethodPatch && strings.HasPrefix(rest, "issues/"):
			var n int
			_, _ = sscanIssue(rest, &n)
			var body struct {
				State       string `json:"state"`
				StateReason string `json:"state_reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.State == "closed" && s.closeFails[n] {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			if body.State == "closed" {
				s.closed[n] = body.StateReason
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": body.State})
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/labels"):
			var n int
			_, _ = sscanIssue(rest, &n)
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.added[n] = append(s.added[n], body...)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.Contains(rest, "/labels/"):
			var n int
			tail, _ := sscanIssue(rest, &n)
			name := strings.TrimPrefix(tail, "labels/")
			s.removed[n] = append(s.removed[n], name)
			kept := s.truth[n][:0:0]
			for _, l := range s.truth[n] {
				if !strings.EqualFold(l, name) {
					kept = append(kept, l)
				}
			}
			s.truth[n] = kept
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/comments"):
			var n int
			_, _ = sscanIssue(rest, &n)
			s.comments[n]++
			var cm struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&cm)
			s.commentBodies[n] = append(s.commentBodies[n], cm.Body)
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// sscanIssue parses "issues/<n>/<tail>" and returns tail.
func sscanIssue(rest string, n *int) (string, bool) {
	rest = strings.TrimPrefix(rest, "issues/")
	num, tail, _ := strings.Cut(rest, "/")
	v := 0
	for _, r := range num {
		if r < '0' || r > '9' {
			return "", false
		}
		v = v*10 + int(r-'0')
	}
	*n = v
	return tail, true
}

// seed makes the fresh read agree with the enumeration payload.
func (s *issueStageServer) seed(result *ActionableResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range IssueStageInputs(result) {
		s.truth[in.Number] = append([]string(nil), in.Labels...)
	}
}

func (s *issueStageServer) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.added {
		if len(a) > 0 {
			n++
		}
	}
	for _, r := range s.removed {
		n += len(r)
	}
	return n
}

// syncSeeded runs the reconciler against a payload the fresh read agrees with.
func syncSeeded(t *testing.T, fake *issueStageServer, c *Client, result *ActionableResult, store *stagelabels.Store, now time.Time) IssueStageResult {
	t.Helper()
	fake.seed(result)
	// The freshness guard's confirming GET honours the process-global
	// slow-start caution window and REST budget; clear any left by earlier
	// tests so a close is not deferred by another test's rate-limit replay.
	ResetRateLimitPacingForTest()
	resetRESTAccountingForTest(t, now)
	return SyncIssueStageLabels(context.Background(), c, result, store, now, testLogger())
}

func issueStageTestClient(t *testing.T, srv *httptest.Server, enabled bool) *Client {
	t.Helper()
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	c.SetIssueStageLabels(func() IssueStagePolicy { return IssueStagePolicy{Enabled: enabled} })
	return c
}

func stageNow() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }

func TestIssueStageFor(t *testing.T) {
	prStage := func(repo string, number int) stagelabels.Stage {
		if repo == "acme/widgets" && number == 99 {
			return stagelabels.MergeQueue
		}
		return stagelabels.None
	}
	cases := []struct {
		name    string
		in      IssueStageInput
		want    stagelabels.Stage
		covered bool
	}{
		{"actionable", IssueStageInput{}, stagelabels.Actionable, false},
		{"live human claim", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: "human"}, stagelabels.Claimed, false},
		{"live contributor claim", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: "contributor"}, stagelabels.Claimed, false},
		// hivecommons/hive#13069: an agent claim is 020 until it carries PR
		// evidence; 030 promises a PR.
		{"live agent claim", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent)}, stagelabels.Claimed, false},
		{"live agent claim with a PR under way", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent), PRStarted: true}, stagelabels.PRInProgress, false},
		{"in-progress sweep", IssueStageInput{Bucket: inProgressWorkBucket}, stagelabels.Claimed, false},
		{"in-progress sweep with a PR under way", IssueStageInput{Bucket: inProgressWorkBucket, PRStarted: true}, stagelabels.PRInProgress, false},
		{"covered, PR not yet staged", IssueStageInput{Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 5}}, stagelabels.CoveredByPR, false},
		// hivecommons/hive#12451: the PR's stage is narrated, never mirrored
		// as the issue's label — 050+ are PR-only stages.
		{"covered, PR staged: stays 040", IssueStageInput{Repo: "acme/widgets", Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 99}}, stagelabels.CoveredByPR, true},
		{"covered, cross-repo PR staged: stays 040", IssueStageInput{Repo: "acme/other", Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 99, PRRepo: "acme/widgets"}}, stagelabels.CoveredByPR, true},
		// hivecommons/hive#15008: a live "Refs #N" claim (#13747) is 040 as
		// "addresses part of it", never "covers it".
		{"partial: Refs claim is 040 but not a cover", IssueStageInput{Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 5, Reference: true, Decision: ClaimDecisionCoveredByOpenPR}}, stagelabels.CoveredByPR, false},
		{"likely-done is no stage", IssueStageInput{Labels: []string{LikelyDoneLabel}}, stagelabels.None, false},
		{"filtered out", IssueStageInput{Bucket: "filtered"}, stagelabels.None, false},
		// Owner decision: an issue parked on a person wears 045, never 085
		// (085 is PR-only); needs-human implies the park.
		{"needs-human is parked at 045 (stale 085 dropped)", IssueStageInput{Bucket: "filtered", Reason: "needs-human", Labels: []string{"needs-human", stagelabels.NeedsDecision.Label()}}, stagelabels.IssueNeedsDecision, false},
		{"parked by needs-direction (#12481)", IssueStageInput{Bucket: "filtered", Reason: "needs-direction", Labels: []string{"help wanted", "needs-direction", stagelabels.PRInProgress.Label()}}, stagelabels.IssueNeedsDecision, false},
		{"parked by needs-decision", IssueStageInput{Bucket: "filtered", Reason: "needs-decision", Labels: []string{"needs-decision"}}, stagelabels.IssueNeedsDecision, false},
		{"parked by legacy 085 on an issue", IssueStageInput{Bucket: "filtered", Reason: PRNeedsDecisionStageLabel, Labels: []string{PRNeedsDecisionStageLabel}}, stagelabels.IssueNeedsDecision, false},
		{"renamed needs-human bucket is parked at 045", IssueStageInput{Bucket: "filtered", Reason: "needs-human", Labels: []string{"triage/needs-maintainer"}}, stagelabels.IssueNeedsDecision, false},
		{"parked label still on a claimed issue", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent), Labels: []string{"needs-spec"}}, stagelabels.IssueNeedsDecision, false},
		{"exempt label is not parked", IssueStageInput{Bucket: "filtered", Reason: "Exempt label", Labels: []string{"wontfix"}}, stagelabels.None, false},
		// hivecommons/hive#13268: the reporter-trust gate's bucket is
		// "actionable but untriaged" — 015, not "outside the pipeline".
		{"reporter triage", IssueStageInput{Bucket: "reporter_triage"}, stagelabels.TriageNeeded, false},
		// hivecommons/hive#13012: a held issue is paused, not parked — hold
		// outranks needs-human / needs-decision / needs-spec and the issue
		// shows as "Open · on hold", never 045. #9846's label set first.
		{"held: hold + needs-human (#9846) is Open · on hold", IssueStageInput{Held: true, Labels: []string{"hold", "needs-human", stagelabels.IssueNeedsDecision.Label()}}, stagelabels.None, false},
		{"held: hold alone", IssueStageInput{Held: true, Labels: []string{"hold"}}, stagelabels.None, false},
		{"held: hold read from labels without the enumeration flag", IssueStageInput{Labels: []string{"hold", "needs-human"}}, stagelabels.None, false},
		{"held: do-not-merge alone", IssueStageInput{Bucket: "filtered", Reason: "Exempt label", Labels: []string{"do-not-merge"}}, stagelabels.None, false},
		{"held: do-not-merge/* + needs-spec", IssueStageInput{Labels: []string{"do-not-merge/blocked-paths", "needs-spec"}}, stagelabels.None, false},
		{"held: hive-pause/<id> alone", IssueStageInput{Held: true, Labels: []string{"hive-pause/hv-test"}}, stagelabels.None, false},
		{"not held: provenance hive/<id> is not a hold", IssueStageInput{Labels: []string{"hive/placeholder-r05x", "needs-human"}}, stagelabels.IssueNeedsDecision, false},
		{"not held: needs-human alone stays 045", IssueStageInput{Bucket: "filtered", Reason: "needs-human", Labels: []string{"needs-human"}}, stagelabels.IssueNeedsDecision, false},
		{"not held: #12964 label set stays actionable", IssueStageInput{Labels: []string{"help wanted", stagelabels.Actionable.Label()}}, stagelabels.Actionable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, covered, reason := IssueStageFor(tc.in, prStage)
			if strings.HasPrefix(tc.name, "held:") && !strings.HasPrefix(reason, "held: ") {
				t.Fatalf("reason %q must name the hold label", reason)
			}
			if partial := strings.HasPrefix(tc.name, "partial:"); partial != stageengine.ReasonIsPartialCover(reason) || (!partial && strings.HasPrefix(tc.name, "covered") && reason != "open PR covers it" && !strings.Contains(reason, " covers it (PR at ")) {
				t.Fatalf("reason %q: partial-cover wording must follow the claim's Reference flag", reason)
			}
			if got != tc.want || covered != tc.covered {
				t.Fatalf("IssueStageFor = %s/%v, want %s/%v", got, covered, tc.want, tc.covered)
			}
			if covered && !strings.Contains(reason, "PR #99 covers it (PR at 100-merge-queue)") {
				t.Fatalf("reason %q must name the covering PR and its stage", reason)
			}
			if got != stagelabels.None && !got.Applies(stagelabels.IssueItem) {
				t.Fatalf("%s is not an issue stage", got)
			}
			if got == stagelabels.NeedsDecision {
				t.Fatal("issue derivation must never emit the PR-only 085")
			}
		})
	}
}

// TestIssueStageFor_TriageNeeded pins the 015 rule (hivecommons/hive#13268):
// an actionable issue is 015-triage-needed when it carries needs-triage (or
// sits in the reporter_triage bucket), or when the hive / an agent filed it
// and no maintainer label (triage/accepted, hive/verified-open,
// approved-direction) is on it. Human-filed issues stay 010, and 015 never
// outranks a claim, a covering PR or a park.
func TestIssueStageFor_TriageNeeded(t *testing.T) {
	const (
		accepted = "triage/accepted"
		awaiting = "needs-triage"
	)
	if triageAwaitingLabel != awaiting {
		t.Fatalf("defaults moved: %q", triageAwaitingLabel)
	}
	cases := []struct {
		name string
		in   IssueStageInput
		want stagelabels.Stage
		why  string
	}{
		{"hive-filed, nothing else (#13262) stays 010", IssueStageInput{AuthorKind: authorkind.Hive, Labels: []string{"kind/review-backlog"}}, stagelabels.Actionable, ""},
		{"agent-filed (clanker) stays 010", IssueStageInput{AuthorKind: authorkind.Clanker}, stagelabels.Actionable, ""},
		{"hive-filed + triage/accepted", IssueStageInput{AuthorKind: authorkind.Hive, Labels: []string{accepted}}, stagelabels.Actionable, ""},
		{"hive-filed + hive/verified-open", IssueStageInput{AuthorKind: authorkind.Hive, Labels: []string{VerifiedOpenLabel}}, stagelabels.Actionable, ""},
		{"hive-filed + approved-direction", IssueStageInput{AuthorKind: authorkind.Hive, Labels: []string{HumanAckLabel}}, stagelabels.Actionable, ""},
		{"hive-filed epic without a maintainer label is 010 (umbrella, claim not staged)", IssueStageInput{AuthorKind: authorkind.Hive, Labels: []string{"Epic"}, Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent)}, stagelabels.Actionable, ""},
		{"human-filed, no labels", IssueStageInput{AuthorKind: authorkind.Human}, stagelabels.Actionable, ""},
		{"human-filed epic + verified-open (#12999)", IssueStageInput{AuthorKind: authorkind.Human, Labels: []string{"Epic", VerifiedOpenLabel}}, stagelabels.Actionable, ""},
		{"unknown author is read as human", IssueStageInput{}, stagelabels.Actionable, ""},
		{"human-filed + needs-triage", IssueStageInput{AuthorKind: authorkind.Human, Labels: []string{awaiting}}, stagelabels.TriageNeeded, "carries needs-triage"},
		{"needs-triage beats triage/accepted", IssueStageInput{AuthorKind: authorkind.Human, Labels: []string{awaiting, accepted}}, stagelabels.TriageNeeded, "carries needs-triage"},
		{"needs-triage is case-insensitive", IssueStageInput{Labels: []string{"Needs-Triage"}}, stagelabels.TriageNeeded, "carries needs-triage"},
		{"reporter_triage bucket, human", IssueStageInput{Bucket: workBucketReporterTriage, AuthorKind: authorkind.Human, Labels: []string{accepted}}, stagelabels.TriageNeeded, "reporter-trust gate"},
		// 015 is a tracking state, not a gate: everything that outranks
		// 010 outranks it too.
		{"claim from an untriaged issue is 020", IssueStageInput{AuthorKind: authorkind.Hive, Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent)}, stagelabels.Claimed, ""},
		{"claim with PR evidence is 030", IssueStageInput{AuthorKind: authorkind.Hive, Labels: []string{awaiting}, Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent), PRStarted: true}, stagelabels.PRInProgress, ""},
		{"covered untriaged issue is 040", IssueStageInput{AuthorKind: authorkind.Hive, Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 5}}, stagelabels.CoveredByPR, ""},
		{"parked untriaged issue is 045", IssueStageInput{AuthorKind: authorkind.Hive, Labels: []string{awaiting, "needs-human"}}, stagelabels.IssueNeedsDecision, ""},
		{"held untriaged issue is Open · on hold", IssueStageInput{AuthorKind: authorkind.Hive, Held: true, Labels: []string{"hold"}}, stagelabels.None, ""},
		{"likely-done untriaged issue leaves the pipeline", IssueStageInput{AuthorKind: authorkind.Hive, Labels: []string{LikelyDoneLabel}}, stagelabels.None, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, covered, reason := IssueStageFor(tc.in, nil)
			if got != tc.want || covered {
				t.Fatalf("IssueStageFor = %s/%v %q, want %s", got, covered, reason, tc.want)
			}
			if got == stagelabels.TriageNeeded && (!strings.HasPrefix(reason, "triage needed: ") || !strings.Contains(reason, tc.why)) {
				t.Fatalf("reason %q must explain the triage verdict (%q)", reason, tc.why)
			}
			if got == stagelabels.Actionable && strings.Contains(reason, "triage") {
				t.Fatalf("010 reason %q must not mention triage", reason)
			}
		})
	}
}

// issueAuthorKind maps the enumeration's human verdict onto the stage
// input; IssueStageInputs carries it for every actionable item.
func TestIssueStageInputs_CarriesAuthorKind(t *testing.T) {
	res := &ActionableResult{}
	res.Issues.Items = []Issue{
		{Repo: "o/r", Number: 1, Author: "alice", AuthorIsHuman: true},
		{Repo: "o/r", Number: 2, Author: "hive-app[bot]"},
		// No login at all is "could not tell", read as human downstream.
		{Repo: "o/r", Number: 3},
	}
	var got []authorkind.Kind
	for _, in := range IssueStageInputs(res) {
		got = append(got, in.AuthorKind)
	}
	if len(got) != 3 || got[0] != authorkind.Human || got[1] != authorkind.Hive || got[2] != authorkind.Unknown {
		t.Fatalf("author kinds = %v", got)
	}
}

// An issue carrying needs-human and the PR-only 085 (pre-045 park) is
// re-staged in one write: 045 on, 085 off.
func TestSyncIssueStageLabels_NeedsHumanClearsStaleDecisionStage(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{
		Repo:   "acme/widgets",
		Number: 6,
		Labels: []string{issueNeedsHumanLabel, stagelabels.NeedsDecision.Label()},
	}, workBucketFiltered, issueNeedsHumanLabel)

	res := syncSeeded(t, fake, c, result, stagelabels.NewStore("", stageNow), stageNow())
	if res.Items != 1 {
		t.Fatalf("items = %d, want 1", res.Items)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.added[6], ","); got != stagelabels.IssueNeedsDecision.Label() {
		t.Fatalf("needs-human issue must park at 045: added=%v", fake.added[6])
	}
	if got := strings.Join(fake.removed[6], ","); got != stagelabels.NeedsDecision.Label() {
		t.Fatalf("removed = %v, want stale %s cleared", fake.removed[6], stagelabels.NeedsDecision.Label())
	}
	if res.NeedsDecisionRestaged != 1 {
		t.Fatalf("NeedsDecisionRestaged = %d, want 1", res.NeedsDecisionRestaged)
	}
}

// hivecommons/hive#13012: #9846 wore stage/045-needs-decision beside `hold`
// + `needs-human`. The held issue is walked from the Hold list, the 045
// label comes off, the store record is forgotten (so the dashboard reads
// Open · on hold), and no stage label is added. A held issue with no stage
// label costs no write; a hive-pause/<id> hold reaches the classifier
// through the Hold list.
func TestSyncIssueStageLabels_HeldIssueLeaves045(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	store.Observe(stagelabels.Key("acme/widgets", 9846), stagelabels.IssueNeedsDecision, "held for a person: needs-human")
	result := &ActionableResult{}
	result.Hold.Items = []HoldItem{
		{Repo: "acme/widgets", Number: 9846, Type: "issue", Labels: []string{"hold", issueNeedsHumanLabel, stagelabels.IssueNeedsDecision.Label()}},
		{Repo: "acme/widgets", Number: 9735, Type: "issue", Labels: []string{"hold", issueNeedsHumanLabel}},
		{Repo: "acme/widgets", Number: 9700, Type: "issue", Labels: []string{"hive-pause/hv-test"}},
		{Repo: "acme/widgets", Number: 77, Type: "pr", Labels: []string{"hold", stagelabels.NeedsDecision.Label()}},
	}
	inputs := IssueStageInputs(result)
	if len(inputs) != 3 {
		t.Fatalf("inputs = %d, want 3 held issues (the held PR is not an issue)", len(inputs))
	}
	for _, in := range inputs {
		if !in.Held {
			t.Fatalf("issue %d from the Hold list must be Held", in.Number)
		}
		if stage, _, reason := IssueStageFor(in, nil); stage != stagelabels.None || !strings.HasPrefix(reason, "held: ") {
			t.Fatalf("issue %d: stage %s reason %q, want none/held", in.Number, stage, reason)
		}
	}

	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.Items != 3 {
		t.Fatalf("items = %d, want 3", res.Items)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.removed[9846], ","); got != stagelabels.IssueNeedsDecision.Label() {
		t.Fatalf("held issue must shed 045: removed=%v", fake.removed[9846])
	}
	for _, n := range []int{9846, 9735, 9700} {
		if len(fake.added[n]) != 0 {
			t.Fatalf("held issue %d must carry no stage label: added=%v", n, fake.added[n])
		}
	}
	if len(fake.removed[9735])+len(fake.removed[9700])+len(fake.removed[77])+len(fake.added[77]) != 0 {
		t.Fatalf("unexpected writes: removed=%v added=%v", fake.removed, fake.added)
	}
	if _, ok := store.Get(stagelabels.Key("acme/widgets", 9846)); ok {
		t.Fatal("store must forget a held issue so the tracker reads Open · on hold")
	}
}

func issueStageResult() *ActionableResult {
	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{
		{Repo: "acme/widgets", Number: 1, Labels: []string{"bug"}},
	})
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 2, Labels: []string{claims.LegacyLabelClaimed}, ClaimKind: "human"}, workBucketClaimed, "live claim")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 3, Labels: []string{claims.LegacyLabelInProgress}, ClaimKind: string(claims.KindAgent), ClaimPRStarted: true}, workBucketClaimed, "live claim")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 4, Labels: []string{stagelabels.LegacyCoveredByPR}, ClaimContext: &IssueClaimContext{PRNumber: 40}}, workBucketClaimedByPR, "open PR")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 5, Labels: []string{stagelabels.Actionable.Label()}}, "filtered", "Exempt label")
	return result
}

func TestSyncIssueStageLabels_MapsBucketsAndRetiresLegacy(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	store.Observe(stagelabels.Key("acme/widgets", 40), stagelabels.ReviewPending, "contributor-lane:no-verdict")

	res := syncSeeded(t, fake, c, issueStageResult(), store, stageNow())
	if res.Items != 5 {
		t.Fatalf("items = %d, want 5", res.Items)
	}
	want := map[int]struct{ add, remove []string }{
		1: {add: []string{stagelabels.Actionable.Label()}},
		2: {add: []string{stagelabels.Claimed.Label()}, remove: []string{claims.LegacyLabelClaimed}},
		3: {add: []string{stagelabels.PRInProgress.Label()}, remove: []string{claims.LegacyLabelInProgress}},
		4: {add: []string{stagelabels.CoveredByPR.Label()}, remove: []string{stagelabels.LegacyCoveredByPR}}, // PR staged 050, issue stays 040 (hivecommons/hive#12451)
		5: {remove: []string{stagelabels.Actionable.Label()}},
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for n, w := range want {
		if got := fake.added[n]; strings.Join(got, ",") != strings.Join(w.add, ",") {
			t.Errorf("issue %d added %v, want %v", n, got, w.add)
		}
		if got := fake.removed[n]; strings.Join(got, ",") != strings.Join(w.remove, ",") {
			t.Errorf("issue %d removed %v, want %v", n, got, w.remove)
		}
	}
	if fake.lists != 1 {
		t.Fatalf("label family listed %d times, want once per repo per process", fake.lists)
	}
	if len(fake.comments) != 0 {
		t.Fatalf("unexpected comments: %v", fake.comments)
	}
}

func TestSyncIssueStageLabels_IdempotentAndDisabled(t *testing.T) {
	fake := newIssueStageServer(t)
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 1, Labels: []string{"bug", stagelabels.Actionable.Label()}}})
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 2, Labels: []string{stagelabels.Claimed.Label(), "hive/claimed-by-bob"}, ClaimKind: "human"}, workBucketClaimed, "live claim")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 4, Labels: []string{stagelabels.CoveredByPR.Label()}, ClaimContext: &IssueClaimContext{PRNumber: 40}}, workBucketClaimedByPR, "open PR")

	c := issueStageTestClient(t, fake.srv, true)
	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.Writes != 0 || fake.writes() != 0 {
		t.Fatalf("unchanged state produced %d writes (%v / %v)", fake.writes(), fake.added, fake.removed)
	}
	if fake.lists != 0 {
		t.Fatal("no writes planned must not provision the label family")
	}

	off := issueStageTestClient(t, fake.srv, false)
	result.Issues.Items[0].Labels = []string{"bug"}
	if res := SyncIssueStageLabels(context.Background(), off, result, store, stageNow(), testLogger()); res.Items != 0 || fake.writes() != 0 {
		t.Fatalf("disabled policy must be a no-op, got %+v", res)
	}
}

// hivecommons/hive#12451: issue #12451 carried 010 + 040 + 050 (050 mirrored
// from PR #12495 by the pre-fix reconciler). Covered by a staged PR, the
// issue is written exactly 040; the PR's stage lives in the reason, and the
// issue's store record never holds a PR stage (so a PR falling 100 → 060
// does not read as the issue regressing).
func TestSyncIssueStageLabels_CoveredIssueStaysAt040(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	store.Observe(stagelabels.Key("acme/widgets", 40), stagelabels.MergeQueue, "soaking")
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 4, Labels: []string{stagelabels.Actionable.Label(), stagelabels.CoveredByPR.Label(), stagelabels.ReviewPending.Label(), stagelabels.StalledLabel}, ClaimContext: &IssueClaimContext{PRNumber: 40}}, workBucketClaimedByPR, "open PR")

	syncSeeded(t, fake, c, result, store, stageNow())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := fake.added[4]; len(got) != 0 {
		t.Fatalf("added %v, want nothing: 040 is already there and 100 is a PR stage", got)
	}
	if got := strings.Join(fake.removed[4], ","); got != strings.Join([]string{stagelabels.Actionable.Label(), stagelabels.ReviewPending.Label(), stagelabels.StalledLabel}, ",") {
		t.Fatalf("removed %v: every other owned label must come off (a covered issue carries no health)", fake.removed[4])
	}
	rec, ok := store.Get(stagelabels.Key("acme/widgets", 4))
	if !ok || rec.Stage != stagelabels.CoveredByPR || !strings.Contains(rec.Reason, "PR #40 covers it (PR at 100-merge-queue)") {
		t.Fatalf("issue record %+v: must hold 040 with the PR's stage in the reason", rec)
	}
	if rec.CoveringPR != stagelabels.Key("acme/widgets", 40) {
		t.Fatalf("covering PR = %q, want the sweep to remember acme/widgets#40 for the out-of-cycle fact path", rec.CoveringPR)
	}
}

func TestSyncIssueStageLabels_LikelyDoneAndHealth(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	now := stageNow()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	key := stagelabels.Key("acme/widgets", 7)

	// Cycle 1: an agent claim with a PR under way → 030.
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 7, ClaimKind: string(claims.KindAgent), ClaimPRStarted: true}, workBucketClaimed, "live claim")
	syncSeeded(t, fake, c, result, store, clock)
	if rec, ok := store.Get(key); !ok || rec.Stage != stagelabels.PRInProgress {
		t.Fatalf("record after cycle 1 = %+v", rec)
	}

	// Cycle 2, two hours on and no PR: the claim lapsed, the issue is
	// actionable again → 010 and one explanatory comment. The backward
	// move is recorded in the store's history, not as a label
	// (stage/regressed is retired, hivecommons/hive#12534).
	clock = now.Add(2 * time.Hour)
	result = &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{stagelabels.PRInProgress.Label()}}})
	syncSeeded(t, fake, c, result, store, clock)
	fake.mu.Lock()
	if got := fake.added[7]; strings.Join(got, ",") != stagelabels.PRInProgress.Label()+","+stagelabels.Actionable.Label() {
		t.Fatalf("adds = %v", got)
	}
	if fake.comments[7] != 1 {
		t.Fatalf("regression comments = %d, want 1", fake.comments[7])
	}
	fake.mu.Unlock()
	if rec, _ := store.Get(key); len(rec.History) != 2 || !rec.History[1].Regressed || rec.History[1].From != stagelabels.PRInProgress {
		t.Fatalf("history = %+v", rec.History)
	}

	// Cycle 3, a day later still actionable: the actionable dwell (24h) is
	// exceeded → stalled; a stale stage/regressed left by an older hive is
	// stripped by the sweep.
	clock = now.Add(2*time.Hour + 25*time.Hour)
	result = &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{stagelabels.Actionable.Label(), stagelabels.RegressedLabel}}})
	syncSeeded(t, fake, c, result, store, clock)
	fake.mu.Lock()
	if fake.comments[7] != 1 {
		t.Fatalf("regression comment must not repeat, got %d", fake.comments[7])
	}
	if added := fake.added[7]; added[len(added)-1] != stagelabels.StalledLabel || len(added) != 3 {
		t.Fatalf("stalled expected after the dwell: adds = %v", added)
	}
	if removed := fake.removed[7]; removed[len(removed)-1] != stagelabels.RegressedLabel {
		t.Fatalf("retired stage/regressed must be stripped: removed = %v", removed)
	}
	fake.mu.Unlock()

	// Cycle 4: a PR merged; the issue stays open as likely-done → no stage,
	// every owned label off, record forgotten.
	result = &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{stagelabels.Actionable.Label(), stagelabels.StalledLabel, LikelyDoneLabel}, ClaimContext: &IssueClaimContext{PRNumber: 70, MergedPR: true}}})
	syncSeeded(t, fake, c, result, store, clock)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.removed[7], ","); got != stagelabels.PRInProgress.Label()+","+stagelabels.RegressedLabel+","+stagelabels.Actionable.Label()+","+stagelabels.StalledLabel {
		t.Fatalf("removed = %v", fake.removed[7])
	}
	if _, ok := store.Get(key); ok {
		t.Fatal("likely-done issue must be forgotten by the store")
	}
}

func TestSweepInProgressIssues_TriggersOnStageLabels(t *testing.T) {
	for _, trigger := range []string{claims.LabelPRInProgress, claims.LabelClaimed, claims.LegacyLabelInProgress} {
		t.Run(trigger, func(t *testing.T) {
			server, removed := sweepServer(t, []string{InProgressCommentBody("scanner", "hive-b", inProgressNow, inProgressNow.Add(time.Hour))})
			c := newTestClient(t, server, "acme", []string{"widgets"})
			enableInProgress(c, "hive-a", 2*time.Hour)
			issue := inProgressIssue()
			issue.Labels = []string{"bug", trigger}
			result := &ActionableResult{Issues: IssueResultFromItems([]Issue{issue})}
			if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, nil); withheld != 1 {
				t.Fatalf("withheld = %d, want 1: %s must trigger the marker read", withheld, trigger)
			}
			if len(*removed) != 0 {
				t.Fatalf("live marker must keep the label, removed %v", *removed)
			}
			// Lapsed marker: the trigger label comes off, whichever spelling.
			server2, removed2 := sweepServer(t, []string{InProgressCommentBody("scanner", "hive-b", inProgressNow.Add(-3*time.Hour), inProgressNow.Add(-time.Hour))})
			c2 := newTestClient(t, server2, "acme", []string{"widgets"})
			enableInProgress(c2, "hive-a", 2*time.Hour)
			issue2 := inProgressIssue()
			issue2.Labels = []string{"bug", trigger}
			result2 := &ActionableResult{Issues: IssueResultFromItems([]Issue{issue2})}
			if withheld := SweepInProgressIssues(context.Background(), c2, result2, inProgressNow, nil); withheld != 0 {
				t.Fatalf("lapsed marker withheld %d", withheld)
			}
			if len(*removed2) != 1 || !strings.HasSuffix((*removed2)[0], url.PathEscape(trigger)) && !strings.HasSuffix((*removed2)[0], trigger) {
				t.Fatalf("removed = %v, want %s", *removed2, trigger)
			}
			if got := result2.Issues.Items[0].Labels; len(got) != 1 || got[0] != "bug" {
				t.Fatalf("labels after sweep = %v", got)
			}
		})
	}
}

// hivecommons/hive#12512: the list payload said stage/010 and the reconciler
// still wants 010, but between the list and the write a claim hook put
// stage/020 on the issue. Planning from the payload alone would see nothing
// to do; Enforce re-reads only when a write is due, so the pass that does
// write (here: the legacy label coming off) finds both stages and leaves
// exactly the wanted one, counting the heal.
func TestSyncIssueStageLabels_HealsDuplicateStageLabels(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)

	// Payload: 010 + 020 both present (the duplicate was visible to the
	// enumeration), ledger says claimed → want 020 only.
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 12, Labels: []string{"bug", stagelabels.Actionable.Label(), stagelabels.Claimed.Label(), "hive/claimed-by-ann"}, ClaimKind: "contributor"}, workBucketClaimed, "live claim")
	fake.seed(result)
	// Truth drifted further: a third stage landed since the list.
	fake.mu.Lock()
	fake.truth[12] = append(fake.truth[12], stagelabels.PRInProgress.Label())
	fake.mu.Unlock()
	res := SyncIssueStageLabels(context.Background(), c, result, store, stageNow(), testLogger())
	fake.mu.Lock()
	if got := strings.Join(fake.removed[12], ","); got != stagelabels.Actionable.Label()+","+stagelabels.PRInProgress.Label() {
		t.Fatalf("removed %v, want 010 and 030 (planned from the fresh read, not the payload)", fake.removed[12])
	}
	if len(fake.added[12]) != 0 {
		t.Fatalf("020 already present, nothing to add; added %v", fake.added[12])
	}
	if fake.gets[12] != 1 {
		t.Fatalf("fresh reads = %d, want exactly one", fake.gets[12])
	}
	fake.mu.Unlock()
	if res.Healed != 1 || res.Writes != 2 {
		t.Fatalf("result = %+v, want Healed=1 Writes=2", res)
	}

	// Payload says 010 only and the issue is actionable: no write is due, so
	// no read either — an unchanged issue stays free.
	result = &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 13, Labels: []string{stagelabels.Actionable.Label()}}})
	fake.seed(result)
	fake.mu.Lock()
	fake.truth[13] = append(fake.truth[13], stagelabels.Claimed.Label())
	fake.mu.Unlock()
	SyncIssueStageLabels(context.Background(), c, result, store, stageNow(), testLogger())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.gets[13] != 0 || len(fake.removed[13]) != 0 {
		t.Fatalf("unchanged payload must not read or write (gets=%d removed=%v); the next cycle's payload carries the duplicate", fake.gets[13], fake.removed[13])
	}
}

func TestIssueStageFor_LegacyNeedsDirectionStillParks(t *testing.T) {
	in := IssueStageInput{Labels: []string{"needs-direction"}}
	if got, _, _ := IssueStageFor(in, nil); got != stagelabels.IssueNeedsDecision {
		t.Fatalf("stage = %s, want 045", got)
	}
	if !hasHardSuppressIssueLabel(in.Labels) {
		t.Fatal("needs-direction must still hard-suppress")
	}
}

func TestSyncIssueStageLabels_EditsHivePRBodyForDefaultBranchLink(t *testing.T) {
	fake := newIssueStageServer(t)
	fake.prBodies[41] = "## Fix\n\nImplements the issue."
	fake.prBases[41] = "v5"
	fake.prAuthors[41] = "hive[bot]"
	c := issueStageTestClient(t, fake.srv, true)
	c.SetAppBotLogin("hive[bot]")
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 4, Labels: []string{stagelabels.CoveredByPR.Label()}, ClaimContext: &IssueClaimContext{PRNumber: 41, PRURL: "https://x/pr/41"}}, workBucketClaimedByPR, "open PR")

	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.PRBodiesEdited != 1 {
		t.Fatalf("PRBodiesEdited = %d, want 1", res.PRBodiesEdited)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !strings.HasSuffix(fake.prPatches[41], "\n\nFixes #4") {
		t.Fatalf("patched body = %q", fake.prPatches[41])
	}
	if fake.comments[4] != 0 {
		t.Fatalf("comments = %d, want 0 when the PR body was fixed", fake.comments[4])
	}
}

func TestSyncIssueStageLabels_PRLinkRateLimitPausesSweepWithOneLine(t *testing.T) {
	fake := newIssueStageServer(t)
	fake.prRateLimited = true
	c := issueStageTestClient(t, fake.srv, true)
	c.SetAppBotLogin("hive[bot]")
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{}
	for n, pr := range map[int]int{4: 41, 5: 42, 6: 43} {
		appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: n, Labels: []string{stagelabels.CoveredByPR.Label()}, ClaimContext: &IssueClaimContext{PRNumber: pr, PRURL: "https://x/pr/" + strconv.Itoa(pr)}}, workBucketClaimedByPR, "open PR")
	}
	fake.seed(result)
	ResetRateLimitPacingForTest()
	resetRESTAccountingForTest(t, stageNow())
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	SyncIssueStageLabels(context.Background(), c, result, store, stageNow(), logger)

	fake.mu.Lock()
	lookups := fake.prLookups
	fake.mu.Unlock()
	if lookups != 1 {
		t.Fatalf("PR link lookups = %d, want 1 (sweep must stop after the rate limit)", lookups)
	}
	out := buf.String()
	if strings.Contains(out, "level=WARN") && strings.Contains(out, "safeguard failed") {
		t.Fatalf("rate limit logged as WARN per pair:\n%s", out)
	}
	if n := strings.Count(out, "PR issue-link safeguard paused"); n != 1 {
		t.Fatalf("summary lines = %d, want 1:\n%s", n, out)
	}
}

func TestSyncIssueStageLabels_CommentsForPartialPRLinkLimitation(t *testing.T) {
	fake := newIssueStageServer(t)
	fake.prBodies[42] = "## Fix\n\nPart of #4; leaves follow-up work open."
	fake.prBases[42] = "v5"
	fake.prAuthors[42] = "hive[bot]"
	c := issueStageTestClient(t, fake.srv, true)
	c.SetAppBotLogin("hive[bot]")
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 4, Labels: []string{stagelabels.CoveredByPR.Label()}, ClaimContext: &IssueClaimContext{PRNumber: 42, PRURL: "https://x/pr/42", Reference: true}}, workBucketClaimedByPR, "open PR")

	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.Comments != 1 || res.PRBodiesEdited != 0 {
		t.Fatalf("result = %+v, want one issue comment and no PR edit", res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.prPatches[42] != "" {
		t.Fatalf("PR body was edited: %q", fake.prPatches[42])
	}
	body := strings.Join(fake.commentBodies[4], "\n")
	if !strings.Contains(body, "PR #42 covers this issue") || !strings.Contains(body, "GitHub will not show") {
		t.Fatalf("comment body = %q", body)
	}
}

func TestSyncIssueStageLabels_MergedReferencePRMovesIssueBackToActionable(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 64, Labels: []string{stagelabels.PRInProgress.Label()}, ClaimContext: &IssueClaimContext{PRNumber: 67, PRURL: "https://x/pr/67", Reference: true, MergedPR: true}}})

	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.Comments != 1 || res.MergedPRIssuesClosed != 0 {
		t.Fatalf("result = %+v, want one partial comment and no close", res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.closed[64] != "" {
		t.Fatalf("issue was closed: %q", fake.closed[64])
	}
	if got := strings.Join(fake.added[64], ","); got != stagelabels.Actionable.Label() {
		t.Fatalf("added = %v, want actionable", fake.added[64])
	}
	if got := strings.Join(fake.removed[64], ","); got != stagelabels.PRInProgress.Label() {
		t.Fatalf("removed = %v, want stale 030", fake.removed[64])
	}
	body := strings.Join(fake.commentBodies[64], "\n")
	if !strings.Contains(body, "PR #67 has merged") || !strings.Contains(body, "landed partial work") {
		t.Fatalf("comment body = %q", body)
	}
}

func TestSyncIssueStageLabels_MergedFullFixPRClosesIssue(t *testing.T) {
	const marker = "<!-- hive:closed-by-merged-pr pr=67 -->"
	tests := []struct {
		name          string
		priorComments []string
		closeFails    bool
		wantClosed    int
		wantComments  int
		wantBodies    int
	}{
		{name: "first pass comments once and closes", wantClosed: 1, wantComments: 1, wantBodies: 1},
		{name: "close failure keeps the single comment", closeFails: true, wantClosed: 0, wantComments: 1, wantBodies: 1},
		{name: "retry after a failed close does not comment again", priorComments: []string{marker + "\nFixed by #67 (merged)."}, wantClosed: 1, wantComments: 0, wantBodies: 1},
		{name: "another PR's marker does not suppress the note", priorComments: []string{"<!-- hive:closed-by-merged-pr pr=12 -->\nFixed by #12 (merged)."}, wantClosed: 1, wantComments: 1, wantBodies: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newIssueStageServer(t)
			fake.commentBodies[64] = append([]string(nil), tt.priorComments...)
			fake.closeFails[64] = tt.closeFails
			c := issueStageTestClient(t, fake.srv, true)
			store := stagelabels.NewStore("", stageNow)
			result := &ActionableResult{}
			result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 64, Labels: []string{stagelabels.CoveredByPR.Label()}, ClaimContext: &IssueClaimContext{PRNumber: 67, PRURL: "https://x/pr/67", Closing: true, MergedPR: true}}})

			res := syncSeeded(t, fake, c, result, store, stageNow())
			if res.MergedPRIssuesClosed != tt.wantClosed || res.Comments != tt.wantComments {
				t.Fatalf("result = %+v, want closed=%d comments=%d", res, tt.wantClosed, tt.wantComments)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if tt.wantClosed == 1 && fake.closed[64] != IssueStateReasonCompleted {
				t.Fatalf("closed reason = %q, want completed", fake.closed[64])
			}
			if tt.wantClosed == 0 && fake.closed[64] != "" {
				t.Fatalf("issue closed = %q, want it left open after the failed close", fake.closed[64])
			}
			if len(fake.commentBodies[64]) != tt.wantBodies {
				t.Fatalf("comment bodies = %q, want %d", fake.commentBodies[64], tt.wantBodies)
			}
			if tt.wantComments == 1 {
				body := fake.commentBodies[64][len(fake.commentBodies[64])-1]
				if !strings.HasPrefix(body, marker) || !strings.Contains(body, "Fixed by #67") || !strings.Contains(body, "full fix") {
					t.Fatalf("comment body = %q", body)
				}
			}
		})
	}
}

// hivecommons/hive#13316: #13221 was covered by open PR #13272 but wore no
// stage label — the claim release had stripped stage/040 and no cycle put
// it back. annotateIssueWithClaim had added the label to the in-memory
// issue, so the sweep planned against a payload that already "had" it.
// The sweep must plan against the labels GitHub returned and write 040.
func TestSyncIssueStageLabels_WritesCoveredStageHiddenBySyntheticLabel(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	issue := Issue{Repo: "acme/widgets", Number: 13221, Labels: []string{"help wanted", "triage/accepted"}}
	annotateIssueWithClaim(&issue, IssueClaim{Repo: "acme/widgets", Issue: 13221, PRRepo: "acme/widgets", PRNumber: 13272, Closing: true})
	if !issueHasLabel(issue.Labels, CoveredByPRLabel) || issue.ClaimContext == nil || !issue.ClaimContext.CoveredLabelSynthesized {
		t.Fatalf("annotation must add the in-memory label and mark it synthesised: %v %+v", issue.Labels, issue.ClaimContext)
	}
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, issue, workBucketClaimedByPR, "Open hive-authored PR #13272 already claims this issue")

	inputs := IssueStageInputs(result)
	if len(inputs) != 1 || issueHasLabel(inputs[0].Labels, CoveredByPRLabel) {
		t.Fatalf("sweep input must carry the forge labels, got %+v", inputs)
	}
	stage, _, _ := IssueStageFor(inputs[0], nil)
	if stage != stagelabels.CoveredByPR {
		t.Fatalf("stage = %s, want 040", stage)
	}

	res := syncSeeded(t, fake, c, result, store, stageNow())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.added[13221], ","); got != CoveredByPRLabel {
		t.Fatalf("added %q, want %q written to the forge (writes=%d)", got, CoveredByPRLabel, res.Writes)
	}
	if len(fake.removed[13221]) != 0 {
		t.Fatalf("removed %v, want nothing: the issue's other labels are not hive-owned", fake.removed[13221])
	}
}

// The enumeration cycle annotates each covered issue twice — once in
// SyncIssuePRClaimLabels (links / retired-label cleanup) and again in
// FilterClaimedIssues (suppress or keep) — on the same payload. The second
// pass used to find the label the first pass synthesised and grade it as
// GitHub's, so the sweep planned against a payload that already "had" 040
// and never wrote it: hivecommons/hive#14372 and #14371 sat at 010 for an
// hour under open `Fixes #N` PRs. The sweep must still see the forge labels
// and write 040 whichever claim tier produced the cover; a label GitHub
// really returned stays a no-op.
func TestSyncIssueStageLabels_CoveredStageSurvivesDoubleAnnotation(t *testing.T) {
	const issue, pr = 14372, 14375
	cases := []struct {
		name        string
		claim       IssueClaim
		forgeLabels []string
		wantAdded   string
		wantBucket  string
	}{
		{
			name:        "external closing claim is suppressed into claimed_by_pr and staged 040",
			claim:       IssueClaim{Closing: true, ExternalAuthor: true, PRAuthor: "thatguywob"},
			forgeLabels: []string{"help wanted", stagelabels.Actionable.Label()},
			wantAdded:   CoveredByPRLabel,
			wantBucket:  workBucketClaimedByPR,
		},
		{
			name:        "hive closing claim is suppressed into claimed_by_pr and staged 040",
			claim:       IssueClaim{Closing: true, PRAuthor: "hive[bot]"},
			forgeLabels: []string{"help wanted", stagelabels.Actionable.Label()},
			wantAdded:   CoveredByPRLabel,
			wantBucket:  workBucketClaimedByPR,
		},
		{
			name:        "reference claim stays actionable in the payload but is staged 040",
			claim:       IssueClaim{Reference: true, PRAuthor: "hive[bot]"},
			forgeLabels: []string{"help wanted", stagelabels.Actionable.Label()},
			wantAdded:   CoveredByPRLabel,
		},
		{
			name:        "label GitHub really returned is left alone",
			claim:       IssueClaim{Closing: true, ExternalAuthor: true, PRAuthor: "thatguywob"},
			forgeLabels: []string{"help wanted", CoveredByPRLabel},
			wantBucket:  workBucketClaimedByPR,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newIssueStageServer(t)
			c := issueStageTestClient(t, fake.srv, true)
			store := stagelabels.NewStore("", stageNow)
			now := stageNow()
			claim := tc.claim
			claim.Repo, claim.Issue = "acme/widgets", issue
			claim.PRRepo, claim.PRNumber, claim.PRState = "acme/widgets", pr, PRStateOpen
			claim.PRURL = "https://github.com/acme/widgets/pull/14375"
			claim.ObservedAt, claim.FirstObservedAt = now, now
			ledger := NewClaimLedger("", testLogger())
			ledger.SetClock(func() time.Time { return now })
			ledger.Reconcile([]IssueClaim{claim}, true)

			result := &ActionableResult{}
			result.Issues.Items = []Issue{{Repo: "acme/widgets", Number: issue, Labels: append([]string(nil), tc.forgeLabels...)}}
			// The production order: nil client so the link/label side effects
			// are skipped and only the in-memory annotation runs, then the
			// guard that annotates the same payload again.
			SyncIssuePRClaimLabels(context.Background(), nil, result, ledger, testLogger())
			FilterClaimedIssues(result, ledger, nil, testLogger())

			inputs := IssueStageInputs(result)
			if len(inputs) != 1 {
				t.Fatalf("inputs = %+v, want exactly one row", inputs)
			}
			in := inputs[0]
			if in.Bucket != tc.wantBucket {
				t.Fatalf("bucket = %q, want %q", in.Bucket, tc.wantBucket)
			}
			if got, want := issueHasLabel(in.Labels, CoveredByPRLabel), issueHasLabel(tc.forgeLabels, CoveredByPRLabel); got != want {
				t.Fatalf("sweep input labels = %v, want 040 present=%v (forge labels %v)", in.Labels, want, tc.forgeLabels)
			}
			if stage, _, _ := IssueStageFor(in, nil); stage != stagelabels.CoveredByPR {
				t.Fatalf("stage = %s, want 040", stage)
			}

			res := syncSeeded(t, fake, c, result, store, now)
			forgeWrites := fake.writes()
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if got := strings.Join(fake.added[issue], ","); got != tc.wantAdded {
				t.Fatalf("added %q, want %q (writes=%d)", got, tc.wantAdded, res.Writes)
			}
			if tc.wantAdded == "" && (res.Writes != 0 || forgeWrites != 0) {
				t.Fatalf("unchanged covered issue produced writes: %+v (%v / %v)", res, fake.added, fake.removed)
			}
		})
	}
}

// annotateIssueWithClaim is idempotent on the synthesised flag: a second
// annotation of the same payload keeps reporting the label as the hive's.
func TestAnnotateIssueWithClaim_SynthesizedFlagSurvivesReannotation(t *testing.T) {
	claim := IssueClaim{Repo: "acme/widgets", Issue: 1, PRRepo: "acme/widgets", PRNumber: 2, Closing: true}
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{name: "forge lacks the label", labels: []string{"bug"}, want: true},
		{name: "forge carries the label", labels: []string{"bug", CoveredByPRLabel}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := Issue{Repo: "acme/widgets", Number: 1, Labels: tc.labels}
			for pass := 1; pass <= 2; pass++ {
				annotateIssueWithClaim(&issue, claim)
				if issue.ClaimContext == nil || issue.ClaimContext.CoveredLabelSynthesized != tc.want {
					t.Fatalf("pass %d: CoveredLabelSynthesized = %+v, want %v", pass, issue.ClaimContext, tc.want)
				}
			}
		})
	}
}

// The label GitHub really returned is not synthesised, so a covered issue
// already at 040 stays a no-op (no re-read, no write).
func TestSyncIssueStageLabels_RealCoveredLabelIsNotStripped(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	issue := Issue{Repo: "acme/widgets", Number: 5, Labels: []string{"bug", CoveredByPRLabel}}
	annotateIssueWithClaim(&issue, IssueClaim{Repo: "acme/widgets", Issue: 5, PRRepo: "acme/widgets", PRNumber: 50})
	if issue.ClaimContext.CoveredLabelSynthesized {
		t.Fatal("a label the payload carried must not be marked synthesised")
	}
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, issue, workBucketClaimedByPR, "open PR")
	if res := syncSeeded(t, fake, c, result, store, stageNow()); res.Writes != 0 || fake.writes() != 0 {
		t.Fatalf("unchanged covered issue produced writes: %+v (%v / %v)", res, fake.added, fake.removed)
	}
}

func TestForgeIssueLabels(t *testing.T) {
	labels := []string{"bug", CoveredByPRLabel}
	if got := forgeIssueLabels(labels, nil); len(got) != 2 {
		t.Fatalf("nil claim: %v", got)
	}
	if got := forgeIssueLabels(labels, &IssueClaimContext{}); len(got) != 2 {
		t.Fatalf("real label: %v", got)
	}
	if got := forgeIssueLabels(labels, &IssueClaimContext{CoveredLabelSynthesized: true}); len(got) != 1 || got[0] != "bug" {
		t.Fatalf("synthesised label must come off: %v", got)
	}
}

func TestForgeIssueLabelsDoesNotAlias(t *testing.T) {
	// Test that forgeIssueLabels does not mutate the caller's backing array.
	// This verifies the fix for hivecommons/hive#13323: removeIssueLabel used
	// to compact in-place (out := labels[:0]), which would mutate the backing
	// array shared with the caller.
	original := []string{"bug", "help wanted", CoveredByPRLabel}
	originalCopy := append([]string(nil), original...)

	result := forgeIssueLabels(original, &IssueClaimContext{CoveredLabelSynthesized: true})

	if !slices.Equal(original, originalCopy) {
		t.Errorf("forgeIssueLabels mutated input: got %v, want %v", original, originalCopy)
	}
	if got := []string{"bug", "help wanted"}; !slices.Equal(result, got) {
		t.Errorf("forgeIssueLabels result: got %v, want %v", result, got)
	}
}

// archivedLabelServer answers label reads from existing and refuses every
// label create/edit with GitHub's archived-repository 403.
func archivedLabelServer(t *testing.T, existing string, posts, patches *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost, http.MethodPatch:
			mu.Lock()
			if r.Method == http.MethodPost {
				*posts++
			} else {
				*patches++
			}
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Repository was archived so is read-only."}`))
		default:
			_, _ = w.Write([]byte(existing))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureStageLabelFamilyArchivedRepoMemoised(t *testing.T) {
	var mu sync.Mutex
	posts, patches := 0, 0
	srv := archivedLabelServer(t, `[]`, &posts, &patches, &mu)
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	if err := c.ensureStageLabelFamily(context.Background(), "acme/widgets"); err == nil {
		t.Fatal("want the archived refusal surfaced")
	}
	if !c.stageArchived.has("acme/widgets") {
		t.Fatal("archived repo not memoised")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 || patches != 0 {
		t.Fatalf("label creates = %d, edits = %d, want 1 create and no edits", posts, patches)
	}
}

// An archived repo whose stage labels exist with a drifted colour is refused
// on EditLabel rather than CreateLabel; it must be memoised the same way.
func TestEnsureStageLabelFamilyArchivedRepoMemoisedOnEdit(t *testing.T) {
	var mu sync.Mutex
	posts, patches := 0, 0
	def := stagelabels.AllLabelDefs()[0]
	existing, _ := json.Marshal([]map[string]string{{"name": def.Name, "color": "000000"}})
	srv := archivedLabelServer(t, string(existing), &posts, &patches, &mu)
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	if err := c.ensureStageLabelFamily(context.Background(), "acme/widgets"); err == nil {
		t.Fatal("want the archived refusal surfaced")
	}
	if !c.stageArchived.has("acme/widgets") {
		t.Fatal("archived repo not memoised on the edit path")
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 || patches != 1 {
		t.Fatalf("label creates = %d, edits = %d, want no creates and 1 edit", posts, patches)
	}
}

// hivecommons/hive#14026: the first sweep against an archived repo pays one
// refused create and logs the archived warning; later sweeps skip the repo
// without touching the label API or logging again.
func TestSyncIssueStageLabels_ArchivedRepoSkippedOnLaterSweeps(t *testing.T) {
	fake := newIssueStageServer(t)
	var mu sync.Mutex
	posts := 0
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos/acme/widgets/labels") {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				mu.Lock()
				posts++
				mu.Unlock()
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"Repository was archived so is read-only."}`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
			return
		}
		fake.srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	var logMu sync.Mutex
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(lockedWriter{mu: &logMu, w: &logs}, nil))
	c := NewClientForTest(front.URL, "acme", []string{"widgets"}, logger)
	c.SetIssueStageLabels(func() IssueStagePolicy { return IssueStagePolicy{Enabled: true} })
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 1, Labels: []string{"bug"}}})
	fake.seed(result)
	ResetRateLimitPacingForTest()
	resetRESTAccountingForTest(t, stageNow())

	for sweep := 1; sweep <= 2; sweep++ {
		SyncIssueStageLabelsWith(context.Background(), c, result, store, stageNow(), logger, nil)
	}
	mu.Lock()
	gotPosts := posts
	mu.Unlock()
	if gotPosts != 1 {
		t.Fatalf("label creates across two sweeps = %d, want 1 (archived repo skipped after the first refusal)", gotPosts)
	}
	if fake.writes() != 0 {
		t.Fatalf("archived repo must receive no issue label writes, got %v / %v", fake.added, fake.removed)
	}
	logMu.Lock()
	out := logs.String()
	logMu.Unlock()
	if n := strings.Count(out, "repository is archived and read-only"); n != 1 {
		t.Fatalf("archived warning logged %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "provisioning failed") {
		t.Fatalf("archived repo must not log the generic provisioning failure:\n%s", out)
	}
}

// lockedWriter serialises slog output written from the sweep.
type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestNoteArchivedWrite(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "acme", []string{"widgets"}, testLogger())
	forbidden := func(msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}, Message: msg}
	}
	if c.noteArchivedWrite("acme/widgets", forbidden("Resource not accessible by integration")) {
		t.Fatal("unrelated 403 treated as archived")
	}
	if c.noteArchivedWrite("acme/widgets", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: "Repository was archived so is read-only."}) {
		t.Fatal("non-403 treated as archived")
	}
	if c.stageArchived.has("acme/widgets") {
		t.Fatal("repo memoised without an archived refusal")
	}
	if !c.noteArchivedWrite("acme/widgets", forbidden("Repository was archived so is read-only.")) {
		t.Fatal("archived refusal not recognised")
	}
	if !c.stageArchived.has("acme/widgets") {
		t.Fatal("archived repo not memoised")
	}
}
