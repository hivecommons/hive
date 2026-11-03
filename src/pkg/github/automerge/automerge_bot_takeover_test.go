package automerge

// github-actions[bot] PR takeover (#12003). These pin the trigger predicate,
// that a takeover re-authors the bot head as one signed-off App commit on the
// merge base, opens a superseding PR and closes the bot PR, that each bot head
// SHA gets exactly one attempt, and that API failures degrade to the
// never-ran hold instead of an error.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// takeoverAPI serves the endpoints takeOverBotPR calls. fail maps a route
// name to the status it answers with instead of success.
type takeoverAPI struct {
	t        *testing.T
	fail     map[string]int
	calls    map[string]int
	commit   map[string]any
	ref      map[string]any
	newPR    map[string]any
	comments []string
	edits    []map[string]any
}

func newTakeoverAPI(t *testing.T, fail map[string]int) *takeoverAPI {
	return &takeoverAPI{t: t, fail: fail, calls: map[string]int{}}
}

func (a *takeoverAPI) route(r *http.Request) string {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/compare/"):
		return "compare"
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/git/commits/"):
		return "get-commit"
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/git/commits":
		return "create-commit"
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/git/refs":
		return "create-ref"
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls":
		return "create-pr"
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/23/comments":
		return "comment"
	case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/widget/pulls/23":
		return "close"
	}
	return ""
}

// handle answers a takeover route and reports whether r was one.
func (a *takeoverAPI) handle(w http.ResponseWriter, r *http.Request) bool {
	route := a.route(r)
	if route == "" {
		return false
	}
	a.calls[route]++
	if status := a.fail[route]; status != 0 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "rejected"})
		return true
	}
	var in map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	switch route {
	case "compare":
		if r.URL.Path != "/repos/acme/widget/compare/main...sha23" {
			a.t.Errorf("compare path = %s, want main...sha23", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"merge_base_commit": map[string]any{"sha": "mergebase"}})
	case "get-commit":
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": "sha23", "tree": map[string]any{"sha": "tree23"}})
	case "create-commit":
		a.commit = in
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": "takeoversha"})
	case "create-ref":
		a.ref = in
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": in["ref"]})
	case "create-pr":
		a.newPR = in
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 77, "html_url": "https://github.com/acme/widget/pull/77"})
	case "comment":
		body, _ := in["body"].(string)
		a.comments = append(a.comments, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": body})
	case "close":
		a.edits = append(a.edits, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 23, "state": "closed"})
	}
	return true
}

// newTakeoverSweepAPI fronts the self-authored sweep mock with takeoverAPI.
func newTakeoverSweepAPI(t *testing.T, pr selfAuthoredPR, takeover *takeoverAPI) *httptest.Server {
	t.Helper()
	var merged []int
	backend := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if takeover.handle(w, r) {
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	return front
}

func takeoverEngine(apiURL string, now time.Time) *Engine {
	c := newAutoMergeSweepClient(apiURL)
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{githubActionsBotLogin: true} }
	c.botTakeoverEnabledFn = func() bool { return true }
	c.now = func() time.Time { return now }
	return c
}

func TestTrySweepTrustedBotTakesOverGitHubActionsPROncePerHead(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "example.test")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.author = githubActionsBotLogin
		p.updatedAt = now.Add(-2 * heldRunNeverStartedGrace).Format(time.RFC3339)
	})
	takeover := newTakeoverAPI(t, nil)
	api := newTakeoverSweepAPI(t, pr, takeover)
	c := takeoverEngine(api.URL, now)
	rec := &recordingBoundary{}
	c.mutation = rec

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if reason != botPRTakenOverReason {
		t.Fatalf("reason = %q, want %q", reason, botPRTakenOverReason)
	}
	msg, _ := takeover.commit["message"].(string)
	if !strings.Contains(msg, "Re-authored from #23 (head sha23)") || !strings.HasSuffix(msg, "Signed-off-by: kubestellar-hive[bot] <kubestellar-hive@example.test>") {
		t.Fatalf("commit message = %q, want provenance and bracket-free App sign-off", msg)
	}
	if takeover.commit["tree"] != "tree23" {
		t.Fatalf("commit tree = %v, want the bot head tree tree23", takeover.commit["tree"])
	}
	if parents, _ := takeover.commit["parents"].([]any); len(parents) != 1 || parents[0] != "mergebase" {
		t.Fatalf("commit parents = %v, want [mergebase]", takeover.commit["parents"])
	}
	if takeover.ref["ref"] != "refs/heads/hive/takeover-23" || takeover.ref["sha"] != "takeoversha" {
		t.Fatalf("created ref = %v, want refs/heads/hive/takeover-23 at takeoversha", takeover.ref)
	}
	if takeover.newPR["head"] != "hive/takeover-23" || takeover.newPR["base"] != "main" || !strings.Contains(takeover.newPR["body"].(string), "Supersedes #23.") {
		t.Fatalf("created PR = %v, want hive/takeover-23 -> main superseding #23", takeover.newPR)
	}
	if len(takeover.comments) != 1 || !strings.HasPrefix(takeover.comments[0], "Superseded by #77") {
		t.Fatalf("comments = %q, want one Superseded by #77", takeover.comments)
	}
	if len(takeover.edits) != 1 || takeover.edits[0]["state"] != "closed" {
		t.Fatalf("PR edits = %v, want the bot PR closed", takeover.edits)
	}
	var kinds []string
	for _, claim := range rec.claims {
		kinds = append(kinds, claim.Kind)
	}
	if strings.Join(kinds, ",") != strings.Join([]string{effects.KindBranchPush, effects.KindPullRequestCreate, effects.KindIssueComment}, ",") {
		t.Fatalf("mutation claim kinds = %v, want branch_push, pull_request_create, issue_comment", kinds)
	}
	if rec.claims[0].Target != "hive/takeover-23" || rec.claims[0].Inputs["head_sha"] != "sha23" {
		t.Fatalf("branch claim = %#v, want hive/takeover-23 for head sha23", rec.claims[0])
	}

	// The bot PR may still be listed open on the next tick: no second takeover.
	_, reason, err = c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("second trySweepSelfAuthoredPR returned error: %v", err)
	}
	if want := requiredCheckNeverRanPrefix + "ci/build"; reason != want {
		t.Fatalf("second reason = %q, want %q", reason, want)
	}
	if takeover.calls["compare"] != 1 || takeover.calls["create-pr"] != 1 {
		t.Fatalf("takeover calls = %v, want exactly one attempt per head", takeover.calls)
	}
}

func TestTrySweepTrustedBotTakeoverOffKeepsNeverStartedHold(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	pr := missingRequiredPR(func(p *selfAuthoredPR) {
		p.author = githubActionsBotLogin
		p.updatedAt = now.Add(-2 * heldRunNeverStartedGrace).Format(time.RFC3339)
	})
	takeover := newTakeoverAPI(t, nil)
	api := newTakeoverSweepAPI(t, pr, takeover)
	c := takeoverEngine(api.URL, now)
	c.botTakeoverEnabledFn = func() bool { return false }

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 23, true)
	if err != nil {
		t.Fatalf("trySweepSelfAuthoredPR returned error: %v", err)
	}
	if want := requiredCheckNeverStartedPrefix + "ci/build"; reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
	if len(takeover.calls) != 0 {
		t.Fatalf("takeover calls = %v, want none when auto_merge.bot_takeover.enabled is off", takeover.calls)
	}
}

func TestBotTakeoverEligible(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * heldRunNeverStartedGrace)
	samePR := func(author string) *gh.PullRequest {
		return &gh.PullRequest{
			User: &gh.User{Login: gh.Ptr(author)},
			Head: &gh.PullRequestBranch{Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
			Base: &gh.PullRequestBranch{Ref: gh.Ptr("main"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		}
	}
	forkPR := samePR(githubActionsBotLogin)
	forkPR.Head.Repo.FullName = gh.Ptr("mallory/widget")
	noBase := samePR(githubActionsBotLogin)
	noBase.Base.Ref = gh.Ptr(" ")
	on := func() bool { return true }
	off := func() bool { return false }
	trusted := func() map[string]bool { return map[string]bool{githubActionsBotLogin: true, "dependabot[bot]": true} }
	for _, tc := range []struct {
		name       string
		enabled    func() bool
		trusted    func() map[string]bool
		pr         *gh.PullRequest
		runs       int
		pushed     time.Time
		minHeadAge time.Duration
		want       bool
	}{
		{name: "eligible", enabled: on, trusted: trusted, pr: samePR(githubActionsBotLogin), pushed: old, want: true},
		{name: "no switch", trusted: trusted, pr: samePR(githubActionsBotLogin), pushed: old},
		{name: "switch off", enabled: off, trusted: trusted, pr: samePR(githubActionsBotLogin), pushed: old},
		{name: "not trusted", enabled: on, trusted: func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }, pr: samePR(githubActionsBotLogin), pushed: old},
		{name: "other trusted bot", enabled: on, trusted: trusted, pr: samePR("dependabot[bot]"), pushed: old},
		{name: "app authored", enabled: on, trusted: trusted, pr: samePR(testHiveAppBotLogin), pushed: old},
		{name: "pull_request runs exist", enabled: on, trusted: trusted, pr: samePR(githubActionsBotLogin), runs: 1, pushed: old},
		{name: "fresh head", enabled: on, trusted: trusted, pr: samePR(githubActionsBotLogin), pushed: now.Add(-time.Minute)},
		{name: "unknown head age", enabled: on, trusted: trusted, pr: samePR(githubActionsBotLogin)},
		{name: "min_head_age longer than grace", enabled: on, trusted: trusted, pr: samePR(githubActionsBotLogin), pushed: old, minHeadAge: 3 * heldRunNeverStartedGrace},
		{name: "fork head", enabled: on, trusted: trusted, pr: forkPR, pushed: old},
		{name: "no base ref", enabled: on, trusted: trusted, pr: noBase, pushed: old},
		{name: "nil pr", enabled: on, trusted: trusted, pushed: old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newAutoMergeSweepClient("http://127.0.0.1:0")
			c.botTakeoverEnabledFn = tc.enabled
			c.trustedBotAuthors = tc.trusted
			c.minHeadAge = tc.minHeadAge
			c.now = func() time.Time { return now }
			if got := c.botTakeoverEligible(tc.pr, tc.runs, tc.pushed); got != tc.want {
				t.Fatalf("botTakeoverEligible = %v, want %v", got, tc.want)
			}
		})
	}
	var nilEngine *Engine
	if nilEngine.botTakeoverEnabled() {
		t.Fatal("nil engine must report the takeover off")
	}
}

func TestBotTakeoverIdempotencyKeyIsPerHead(t *testing.T) {
	c := newAutoMergeSweepClient("http://127.0.0.1:0")
	if c.botTakeoverAttempted("acme", "widget", 23, "abc") {
		t.Fatal("botTakeoverAttempted = true before any attempt")
	}
	steps := []struct {
		name    string
		number  int
		sha     string
		wantNew bool
	}{
		{"first", 23, "abc", true},
		{"repeat tick", 23, "abc", false},
		{"sha case", 23, "ABC", false},
		{"new head", 23, "def", true},
		{"other pr", 24, "abc", true},
	}
	for _, st := range steps {
		if got := c.markBotTakeoverAttempted("acme", "widget", st.number, st.sha); got != st.wantNew {
			t.Fatalf("%s: markBotTakeoverAttempted = %v, want %v", st.name, got, st.wantNew)
		}
	}
	if !c.botTakeoverAttempted("acme", "widget", 23, "ABC") {
		t.Fatal("botTakeoverAttempted = false after an attempt")
	}
	if c.rebaseNudged("acme", "widget", 23, "abc") || !c.markRebaseNudged("acme", "widget", 23, "abc") {
		t.Fatal("takeover key must not collide with the rebase-nudge key")
	}
}

func TestTakeOverBotPRDegradesOnAPIErrors(t *testing.T) {
	pr := &gh.PullRequest{
		Title: gh.Ptr("docs: refresh recordings"),
		User:  &gh.User{Login: gh.Ptr(githubActionsBotLogin)},
		Base:  &gh.PullRequestBranch{Ref: gh.Ptr("main")},
	}
	held := requiredCheckNeverRanPrefix + "ci/build"
	for _, tc := range []struct {
		name      string
		fail      map[string]int
		noLogin   bool
		want      string
		wantCalls map[string]int
	}{
		{name: "no app login", noLogin: true, want: held, wantCalls: map[string]int{}},
		{name: "compare fails", fail: map[string]int{"compare": 404}, want: held, wantCalls: map[string]int{"compare": 1}},
		{name: "head tree fails", fail: map[string]int{"get-commit": 500}, want: held, wantCalls: map[string]int{"compare": 1, "get-commit": 1}},
		{name: "commit fails", fail: map[string]int{"create-commit": 422}, want: held, wantCalls: map[string]int{"compare": 1, "get-commit": 1, "create-commit": 1}},
		{name: "branch exists", fail: map[string]int{"create-ref": 422}, want: held, wantCalls: map[string]int{"compare": 1, "get-commit": 1, "create-commit": 1, "create-ref": 1}},
		{name: "pr create fails", fail: map[string]int{"create-pr": 422}, want: held, wantCalls: map[string]int{"compare": 1, "get-commit": 1, "create-commit": 1, "create-ref": 1, "create-pr": 1}},
		{name: "comment and close fail", fail: map[string]int{"comment": 403, "close": 403}, want: botPRTakenOverReason, wantCalls: map[string]int{"compare": 1, "get-commit": 1, "create-commit": 1, "create-ref": 1, "create-pr": 1, "comment": 1, "close": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			takeover := newTakeoverAPI(t, tc.fail)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !takeover.handle(w, r) {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			if tc.noLogin {
				c.transport.(*hgithub.Client).SetAppBotLogin("")
			}
			for tick := 1; tick <= 2; tick++ {
				got := c.takeOverBotPR(context.Background(), "widget", "acme", "widget", pr, 23, "sha23", "ci/build")
				want := tc.want
				if tick > 1 {
					want = held
				}
				if got != want {
					t.Fatalf("tick %d: takeOverBotPR = %q, want %q", tick, got, want)
				}
			}
			for route, n := range tc.wantCalls {
				if takeover.calls[route] != n {
					t.Fatalf("calls[%s] = %d, want %d (all calls %v)", route, takeover.calls[route], n, takeover.calls)
				}
			}
			if len(takeover.calls) != len(tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", takeover.calls, tc.wantCalls)
			}
		})
	}
}

func TestBotTakeoverMessageAndBody(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	pr := &gh.PullRequest{Title: gh.Ptr(" docs: refresh recordings "), Body: gh.Ptr("Weekly refresh.\n")}
	msg := botTakeoverCommitMessage(pr, 331, "0aa1303", "hivecommons-hive[bot]")
	want := "docs: refresh recordings\n\nRe-authored from #331 (head 0aa1303) by the hive App so pull_request workflows run.\n\nSigned-off-by: hivecommons-hive[bot] <hivecommons-hive@hive.kubestellar.io>"
	if msg != want {
		t.Fatalf("commit message = %q, want %q", msg, want)
	}
	if got := botTakeoverCommitMessage(&gh.PullRequest{}, 331, "0aa1303", "hivecommons-hive[bot]"); !strings.HasPrefix(got, "Re-author #331\n\n") {
		t.Fatalf("untitled commit message = %q, want Re-author #331 headline", got)
	}
	body := botTakeoverPRBody(pr, 331, "Lint and build")
	if !strings.HasPrefix(body, "Weekly refresh.\n\n---\n\nSupersedes #331.") || !strings.Contains(body, "`Lint and build`") {
		t.Fatalf("PR body = %q, want original body then the supersession note", body)
	}
	if got := botTakeoverPRBody(&gh.PullRequest{}, 331, "Lint and build"); !strings.HasPrefix(got, "Supersedes #331.") {
		t.Fatalf("empty-body PR body = %q, want the supersession note only", got)
	}
}
