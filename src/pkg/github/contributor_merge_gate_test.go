package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Contributor-PR merge gate (hivecommons/hive#9624): who counts as the hive,
// what counts as a maintainer's approval, and the merge relay refusing a
// contributor PR that has neither the operator's opt-in nor that approval.

const (
	gateAppLogin     = "hive-app[bot]"
	gateAIAuthor     = "hive-ai"
	gateTrustedBot   = "dependabot[bot]"
	gateContributor  = "outside-dev"
	gateMaintainer   = "maintainer-1"
	gateReadOnlyUser = "drive-by-reviewer"
	gateHead         = "abc" // greenFixture's default head
	gatePRNumber     = 42
)

func gateIdentity() HiveIdentity {
	return HiveIdentity{AIAuthor: gateAIAuthor, AppLogin: gateAppLogin}
}

func gateTrustedBots() map[string]bool { return map[string]bool{gateTrustedBot: true} }

func TestClassifyMergeAuthor(t *testing.T) {
	cases := []struct {
		author      string
		appAuthored bool
		want        MergeAuthorClass
	}{
		{"anything", true, MergeAuthorHive},
		{gateAppLogin, false, MergeAuthorHive},
		{"HIVE-APP[bot]", false, MergeAuthorHive},
		{"app/hive-app", false, MergeAuthorHive},
		{"App/Hive-App", false, MergeAuthorHive},
		{gateAIAuthor, false, MergeAuthorHive},
		{" Hive-AI ", false, MergeAuthorHive},
		{gateTrustedBot, false, MergeAuthorTrustedBot},
		{"Dependabot[bot]", false, MergeAuthorTrustedBot},
		{"app/dependabot", false, MergeAuthorTrustedBot},
		{gateContributor, false, MergeAuthorContributor},
		{"renovate[bot]", false, MergeAuthorContributor}, // a bot the operator did not list
		{"", false, MergeAuthorContributor},              // unknown: fail closed
		{"   ", false, MergeAuthorContributor},
		{"app/", false, MergeAuthorContributor},
	}
	for _, tc := range cases {
		if got := ClassifyMergeAuthor(tc.author, tc.appAuthored, gateIdentity(), gateTrustedBots()); got != tc.want {
			t.Errorf("ClassifyMergeAuthor(%q, app=%v) = %q, want %q", tc.author, tc.appAuthored, got, tc.want)
		}
	}
	// No identity and no trusted bots: only the enumerator's App finding is
	// the hive, everyone else is a contributor.
	if got := ClassifyMergeAuthor(gateAIAuthor, false, HiveIdentity{}, nil); got != MergeAuthorContributor {
		t.Errorf("with no identity configured %q = %q, want contributor", gateAIAuthor, got)
	}
}

func TestContributorMergeRefusalReason(t *testing.T) {
	off, on := ContributorMergeRefusalReason(false), ContributorMergeRefusalReason(true)
	for _, r := range []string{off, on} {
		if !strings.HasPrefix(r, ContributorMergeReason) {
			t.Errorf("reason %q does not lead with %q", r, ContributorMergeReason)
		}
		if strings.Contains(r, "@") {
			t.Errorf("reason %q carries a mention", r)
		}
	}
	if !strings.Contains(off, "auto_merge.contributor_prs is off") {
		t.Errorf("opt-in-off reason %q does not name the setting", off)
	}
	if !strings.Contains(on, "write access") || !strings.Contains(on, "review-swarm verdict does not count") {
		t.Errorf("opt-in-on reason %q does not say what is missing", on)
	}
}

func TestMaintainerApprovalAt(t *testing.T) {
	pr := PullRequest{Protection: &ProtectionFacts{MaintainerApprovals: []MaintainerApproval{
		{Login: "", CommitSHA: gateHead},
		{Login: gateMaintainer, CommitSHA: "ABC"},
	}}}
	if login, ok := pr.MaintainerApprovalAt(gateHead); !ok || login != gateMaintainer {
		t.Errorf("approval at head = %q/%v, want %s (SHA match is case-insensitive, empty login ignored)", login, ok, gateMaintainer)
	}
	if _, ok := pr.MaintainerApprovalAt("def"); ok {
		t.Error("approval of another commit counted for this head")
	}
	if _, ok := pr.MaintainerApprovalAt(""); ok {
		t.Error("an empty head matched an approval")
	}
	if _, ok := (PullRequest{}).MaintainerApprovalAt(gateHead); ok {
		t.Error("a PR with no facts reported an approval")
	}
}

// The enumeration's one GraphQL query carries the maintainer approvals: only
// a person's APPROVED review, with push access, on a named commit, and not
// this hive's own account, counts.
func TestFetchReviewDecisions_MaintainerApprovals(t *testing.T) {
	review := func(state, typename, login string, canPush bool, oid string) map[string]any {
		r := map[string]any{
			"state":                     state,
			"authorCanPushToRepository": canPush,
			"author":                    map[string]any{"__typename": typename, "login": login},
		}
		if oid != "" {
			r["commit"] = map[string]any{"oid": oid}
		}
		return r
	}
	p := &protectionServer{
		reviewDecision: "APPROVED",
		reviews: []map[string]any{
			review("APPROVED", "User", gateMaintainer, true, "abc123"),
			review("APPROVED", "User", gateReadOnlyUser, false, "abc123"),       // no push access
			review("APPROVED", "Bot", gateAppLogin, true, "abc123"),             // a bot
			review("APPROVED", "User", gateAIAuthor, true, "abc123"),            // this hive's ai_author
			review("APPROVED", "Mannequin", "imported", true, "abc123"),         // not a person's account
			review("APPROVED", "User", "no-commit", true, ""),                   // commit unknown
			review("CHANGES_REQUESTED", "User", "maintainer-2", true, "abc123"), // not an approval
		},
	}
	srv := p.start(t)
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetHiveIdentity(gateIdentity())
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123", BaseRef: "main"}}
	c.EnrichReviewSignals(context.Background(), prs)

	if prs[0].Protection == nil {
		t.Fatal("Protection is nil")
	}
	got := prs[0].Protection.MaintainerApprovals
	if len(got) != 1 || got[0].Login != gateMaintainer || got[0].CommitSHA != "abc123" {
		t.Fatalf("maintainer approvals = %+v, want only %s at abc123", got, gateMaintainer)
	}
	if prs[0].Protection.ApprovalsGiven != 6 {
		t.Errorf("ApprovalsGiven = %d, want 6 (the display count is unchanged)", prs[0].Protection.ApprovalsGiven)
	}
	if login, ok := prs[0].MaintainerApprovalAt("abc123"); !ok || login != gateMaintainer {
		t.Errorf("MaintainerApprovalAt = %q/%v", login, ok)
	}
}

// contributorRelayServer is greenFixture plus the review listing, the
// collaborator permission endpoint and a counting merge. writes records every
// non-GET request so a refusal can be shown to make none.
type contributorRelayServer struct {
	fixture     *ciFixture
	reviews     []map[string]any
	permissions map[string]string
	reviewsFail bool
	merges      atomic.Int32
	mu          sync.Mutex
	writes      []string
	reviewReads int
}

func (s *contributorRelayServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			s.mu.Lock()
			s.writes = append(s.writes, r.Method+" "+r.URL.Path)
			s.mu.Unlock()
		}
		if s.fixture.serveCI(w, r) {
			return
		}
		enc := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/reviews"):
			s.mu.Lock()
			s.reviewReads++
			s.mu.Unlock()
			if s.reviewsFail {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			enc(s.reviews)
		case r.Method == http.MethodGet && strings.Contains(p, "/collaborators/") && strings.HasSuffix(p, "/permission"):
			parts := strings.Split(p, "/")
			login := parts[len(parts)-2]
			perm, ok := s.permissions[login]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			enc(map[string]any{"permission": perm, "user": map[string]any{"login": login}})
		case r.Method == http.MethodPut && strings.HasSuffix(p, "/merge"):
			s.merges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`)
		case r.Method == http.MethodPut && strings.HasSuffix(p, "/update-branch"):
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (s *contributorRelayServer) nonMergeWrites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, w := range s.writes {
		if !strings.HasSuffix(w, "/merge") {
			out = append(out, w)
		}
	}
	return out
}

func restReview(login, userType, state, commit string) map[string]any {
	return map[string]any{
		"user":      map[string]any{"login": login, "type": userType},
		"state":     state,
		"commit_id": commit,
	}
}

type relayRun struct {
	result  MergeResponse
	audits  []string
	denied  bool
	pending bool
}

// runContributorRelay drops one merge request for gatePRNumber pinned at
// gateHead and processes it once with the given policy installed.
func runContributorRelay(t *testing.T, s *contributorRelayServer, policy *ContributorMergePolicy) relayRun {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.appBotLogin = gateAppLogin
	c.SetHiveIdentity(gateIdentity())
	if policy != nil {
		p := *policy
		c.SetContributorMergePolicy(func() ContributorMergePolicy { return p })
	}
	var mu sync.Mutex
	var audits []string
	c.SetAttributionAudit(func(action, detail, agent string) {
		if action == AuditActionMergeRequestRefused {
			mu.Lock()
			audits = append(audits, detail)
			mu.Unlock()
		}
	})

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	t.Cleanup(func() { mergeRequestDirForTest = "" })
	reqPath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: gatePRNumber, Method: "squash", ExpectSHA: gateHead, Agent: "scanner", UpdateBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())

	run := relayRun{result: readMergeResult(t, reqPath), audits: audits}
	if _, err := os.Stat(reqPath + ".denied"); err == nil {
		run.denied = true
	}
	if _, err := os.Stat(reqPath); err == nil {
		run.pending = true
	}
	return run
}

// The issue's second acceptance case: the relay refuses a contributor PR that
// has no human approval, with a clear result file and an audit entry, and
// makes no write at all (not even the requested branch update).
func TestMergeRelay_RefusesContributorPRWithoutApproval(t *testing.T) {
	cases := map[string]ContributorMergePolicy{
		"opt-in off":             {TrustedBots: gateTrustedBots()},
		"opt-in on, no approval": {AllowContributorPRs: true, TrustedBots: gateTrustedBots()},
	}
	for name, policy := range cases {
		t.Run(name, func(t *testing.T) {
			f := greenFixture()
			f.author = gateContributor
			s := &contributorRelayServer{fixture: f, reviews: []map[string]any{
				// A bot's approval and a stale maintainer approval do not count.
				restReview(gateAppLogin, "Bot", "APPROVED", gateHead),
				restReview(gateMaintainer, "User", "APPROVED", "older-head"),
			}, permissions: map[string]string{gateMaintainer: "write", gateAppLogin: "write"}}
			run := runContributorRelay(t, s, &policy)

			if got := s.merges.Load(); got != 0 {
				t.Fatalf("contributor PR merged %d time(s)", got)
			}
			if w := s.nonMergeWrites(); len(w) != 0 {
				t.Errorf("refused request still wrote to the forge: %v", w)
			}
			if run.result.OK || !strings.Contains(run.result.Error, ContributorMergeReason) {
				t.Errorf("result = %+v, want a refusal naming %q", run.result, ContributorMergeReason)
			}
			if want := ContributorMergeRefusalReason(policy.AllowContributorPRs); !strings.Contains(run.result.Error, want) {
				t.Errorf("result error %q does not carry %q", run.result.Error, want)
			}
			if !run.denied {
				t.Error("refused request was not quarantined as .denied")
			}
			if len(run.audits) != 1 {
				t.Fatalf("audits = %v, want one %s entry", run.audits, AuditActionMergeRequestRefused)
			}
			for _, kv := range []string{"repo=o/r", "number=42", "author=" + gateContributor, "reason=" + auditReasonContributorPR, "agent=scanner"} {
				if !strings.Contains(run.audits[0], kv) {
					t.Errorf("audit detail %q missing %q", run.audits[0], kv)
				}
			}
		})
	}
}

// With the opt-in and a maintainer's approval of the pinned head, the relay
// lets the contributor PR through to the usual CI gate and merge. A person
// without write access, and a later dismissal, do not count.
func TestMergeRelay_ContributorPRWithMaintainerApprovalMerges(t *testing.T) {
	policy := ContributorMergePolicy{AllowContributorPRs: true, TrustedBots: gateTrustedBots()}
	f := greenFixture()
	f.author = gateContributor
	s := &contributorRelayServer{fixture: f, reviews: []map[string]any{
		restReview(gateReadOnlyUser, "User", "APPROVED", gateHead),
		restReview(gateMaintainer, "User", "COMMENTED", gateHead),
		restReview(gateMaintainer, "User", "APPROVED", gateHead),
		restReview(gateMaintainer, "User", "COMMENTED", gateHead), // a later comment keeps the approval standing
	}, permissions: map[string]string{gateMaintainer: "maintain", gateReadOnlyUser: "read"}}
	run := runContributorRelay(t, s, &policy)
	if got := s.merges.Load(); got != 1 || !run.result.OK {
		t.Fatalf("merges=%d result=%+v, want the approved contributor PR merged", got, run.result)
	}
	if len(run.audits) != 0 {
		t.Errorf("an allowed merge was audited as refused: %v", run.audits)
	}

	// The maintainer later dismissed their own approval: refused again.
	f2 := greenFixture()
	f2.author = gateContributor
	s2 := &contributorRelayServer{fixture: f2, reviews: []map[string]any{
		restReview(gateMaintainer, "User", "APPROVED", gateHead),
		restReview(gateMaintainer, "User", "DISMISSED", gateHead),
	}, permissions: map[string]string{gateMaintainer: "admin"}}
	run = runContributorRelay(t, s2, &policy)
	if s2.merges.Load() != 0 || run.result.OK {
		t.Fatalf("dismissed approval still merged: result=%+v", run.result)
	}
}

// Regression: the App's own PRs, project.ai_author's and trusted bots' go
// through the relay exactly as before: no review lookup, no refusal.
func TestMergeRelay_HiveAndTrustedBotPRsUnchanged(t *testing.T) {
	policy := ContributorMergePolicy{TrustedBots: gateTrustedBots()}
	for _, author := range []struct{ login, typ string }{
		{gateAppLogin, "Bot"},
		{"Hive-AI", "User"},
		{gateTrustedBot, "Bot"},
	} {
		t.Run(author.login, func(t *testing.T) {
			f := greenFixture()
			f.author, f.authorType = author.login, author.typ
			s := &contributorRelayServer{fixture: f}
			run := runContributorRelay(t, s, &policy)
			if s.merges.Load() != 1 || !run.result.OK {
				t.Fatalf("merges=%d result=%+v, want merged as before", s.merges.Load(), run.result)
			}
			if s.reviewReads != 0 {
				t.Errorf("reviews were read %d time(s) for a hive/trusted-bot PR", s.reviewReads)
			}
			if len(run.audits) != 0 {
				t.Errorf("audited a refusal: %v", run.audits)
			}
		})
	}
}

// A failed lookup is a retryable failed attempt, never an allow and never a
// permanent denial. With no policy installed the gate makes no lookup.
func TestMergeRelay_ContributorGateLookupFailureAndNoPolicy(t *testing.T) {
	policy := ContributorMergePolicy{AllowContributorPRs: true}
	f := greenFixture()
	f.author = gateContributor
	s := &contributorRelayServer{fixture: f, reviewsFail: true}
	run := runContributorRelay(t, s, &policy)
	if s.merges.Load() != 0 || run.result.OK {
		t.Fatalf("lookup failure merged: %+v", run.result)
	}
	if run.denied || !run.pending || run.result.Attempts != 1 {
		t.Errorf("lookup failure: denied=%v pending=%v attempts=%d, want a retryable first attempt", run.denied, run.pending, run.result.Attempts)
	}
	if !strings.Contains(run.result.Error, "contributor gate") {
		t.Errorf("result error %q does not name the gate", run.result.Error)
	}

	// No policy installed: behaves as before this gate (the governor's
	// eligibility binding carries the rule on that path).
	f2 := greenFixture()
	f2.author = gateContributor
	s2 := &contributorRelayServer{fixture: f2}
	run = runContributorRelay(t, s2, nil)
	if s2.merges.Load() != 1 || s2.reviewReads != 0 {
		t.Errorf("no policy: merges=%d reviewReads=%d, want 1 and 0", s2.merges.Load(), s2.reviewReads)
	}
}
