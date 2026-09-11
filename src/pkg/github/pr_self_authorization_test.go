package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// selfAuthIssue is one issue the fake forge serves.
type selfAuthIssue struct {
	Author     string
	AuthorType string // "Bot" for App/bot accounts; "" means User
	Labels     []string
	Assignees  []string
	Comments   []selfAuthComment
	Status     int // non-zero to fail the GET
	FailList   bool
}

type selfAuthComment struct {
	Author     string
	AuthorType string
}

// selfAuthServer fakes the endpoints the gate reads, plus enough of the PR path
// that the watcher end-to-end tests can run through it.
type selfAuthServer struct {
	issues map[int]*selfAuthIssue

	mu            sync.Mutex
	labelsApplied []string
	comments      []string
	listCalls     int
}

func (s *selfAuthServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/repos/o/r"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"r","default_branch":"main"}`)

		case r.Method == "GET" && strings.Contains(p, "/issues/") && strings.HasSuffix(p, "/comments"):
			s.mu.Lock()
			s.listCalls++
			s.mu.Unlock()
			num := issueNumFromPath(p, "/comments")
			issue := s.issues[num]
			if issue == nil || issue.FailList {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			out := make([]map[string]any, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				out = append(out, map[string]any{"user": userJSON(c.Author, c.AuthorType)})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)

		case r.Method == "GET" && strings.Contains(p, "/issues/"):
			num := issueNumFromPath(p, "")
			issue := s.issues[num]
			if issue == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if issue.Status != 0 {
				w.WriteHeader(issue.Status)
				return
			}
			labels := make([]map[string]any, 0, len(issue.Labels))
			for _, l := range issue.Labels {
				labels = append(labels, map[string]any{"name": l})
			}
			assignees := make([]map[string]any, 0, len(issue.Assignees))
			for _, a := range issue.Assignees {
				assignees = append(assignees, userJSON(a, ""))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":    num,
				"user":      userJSON(issue.Author, issue.AuthorType),
				"labels":    labels,
				"assignees": assignees,
				"comments":  len(issue.Comments),
			})

		case r.Method == "POST" && strings.HasSuffix(p, "/labels"):
			body, _ := io.ReadAll(r.Body)
			var labels []string
			_ = json.Unmarshal(body, &labels)
			s.mu.Lock()
			s.labelsApplied = append(s.labelsApplied, labels...)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)

		case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
			body, _ := io.ReadAll(r.Body)
			var c map[string]any
			_ = json.Unmarshal(body, &c)
			s.mu.Lock()
			s.comments = append(s.comments, fmt.Sprint(c["body"]))
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1}`)

		// #5114's content-metadata gate compares the branch before creating.
		// An empty file list is a branch with nothing objectionable in it.
		case r.Method == "GET" && strings.Contains(p, "/compare/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"files":[]}`)

		case r.Method == "GET" && strings.HasSuffix(p, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)

		case r.Method == "POST" && strings.HasSuffix(p, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":583,"html_url":"https://github.com/o/r/pull/583"}`)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func userJSON(login, kind string) map[string]any {
	u := map[string]any{"login": login}
	if kind != "" {
		u["type"] = kind
	}
	return u
}

func issueNumFromPath(path, suffix string) int {
	p := strings.TrimSuffix(path, suffix)
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(p[idx+1:], "%d", &n)
	return n
}

func (s *selfAuthServer) applied() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.labelsApplied...)
}

func (s *selfAuthServer) postedComments() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.comments...)
}

// TestEvaluateSelfAuthorization is the decision table. The incident is the
// "bot-filed, untouched" row; every other row exists so the gate cannot pass it
// by holding everything.
func TestEvaluateSelfAuthorization(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"

	cases := []struct {
		name      string
		issue     *selfAuthIssue // nil means the issue does not exist
		body      string
		declared  []int
		wantHeld  bool
		wantWhyIn string
	}{{
		name:     "the incident: hive filed it, nobody touched it",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot"},
		body:     "Implements phase 1 of #581.\n\nCloses #581",
		wantHeld: true, wantWhyIn: "no human has acknowledged it",
	}, {
		name:     "a person filed the issue",
		issue:    &selfAuthIssue{Author: "hanthor"},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name:     "hive filed it but a person commented",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Comments: []selfAuthComment{{Author: "hanthor"}}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name: "hive filed it and only bots commented",
		issue: &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Comments: []selfAuthComment{
			{Author: "kubestellar-hive[bot]", AuthorType: "Bot"}, {Author: "dependabot[bot]", AuthorType: "Bot"},
		}},
		body:     "Closes #581",
		wantHeld: true,
	}, {
		name:     "hive filed it but a person approved the direction by label",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Labels: []string{"bug", HumanAckLabel}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name:     "hive filed it but a person assigned themselves",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Assignees: []string{"clubanderson"}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name:     "hive filed it and only a bot is assigned",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Assignees: []string{"kubestellar-hive[bot]"}},
		body:     "Closes #581",
		wantHeld: true,
	}, {
		name:     "a non-closing reference is rationale too",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot"},
		body:     "Part of #581 — extracts the first module.",
		wantHeld: true,
	}, {
		name:     "the request's declared issue list counts",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot"},
		body:     "No references in the prose at all.",
		declared: []int{581},
		wantHeld: true,
	}, {
		name:     "no rationale cited at all is a different problem",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot"},
		body:     "Just a change, no issue reference.",
		wantHeld: false,
	}, {
		name:     "an unreadable issue decides nothing",
		issue:    &selfAuthIssue{Author: botLogin, AuthorType: "Bot", Status: http.StatusInternalServerError},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name:     "an issue with no identifiable author decides nothing",
		issue:    &selfAuthIssue{Author: ""},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name: "authorship known, acknowledgements unreadable: hold",
		issue: &selfAuthIssue{Author: botLogin, AuthorType: "Bot", FailList: true,
			Comments: []selfAuthComment{{Author: "hanthor"}}},
		body:     "Closes #581",
		wantHeld: true, wantWhyIn: "could not be read",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &selfAuthServer{issues: map[int]*selfAuthIssue{}}
			if tc.issue != nil {
				srv.issues[581] = tc.issue
			}
			c := NewClientForTest(srv.start(t).URL, "o", nil, prTestLogger())
			c.SetAppBotLogin(botLogin)

			got := c.EvaluateSelfAuthorization(context.Background(), "o/r", "some title", tc.body, tc.declared)
			if got.Held != tc.wantHeld {
				t.Fatalf("Held = %v, want %v (reason %q)", got.Held, tc.wantHeld, got.Reason)
			}
			if tc.wantHeld {
				if got.Issue != 581 || got.Repo != "o/r" {
					t.Errorf("finding named %s#%d, want o/r#581", got.Repo, got.Issue)
				}
				if tc.wantWhyIn != "" && !strings.Contains(got.Reason, tc.wantWhyIn) {
					t.Errorf("Reason = %q, want it to contain %q", got.Reason, tc.wantWhyIn)
				}
			}
		})
	}
}

// TestEvaluateSelfAuthorization_OneHumanRationaleIsEnough pins the multi-issue
// rule. A PR citing both a hive-filed issue and a human-filed one HAS a human
// rationale; requiring every citation to be human-blessed would hold work that
// a person did ask for.
func TestEvaluateSelfAuthorization_OneHumanRationaleIsEnough(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{
		581: {Author: botLogin, AuthorType: "Bot"},
		590: {Author: "hanthor"},
	}}
	c := NewClientForTest(srv.start(t).URL, "o", nil, prTestLogger())
	c.SetAppBotLogin(botLogin)

	got := c.EvaluateSelfAuthorization(context.Background(), "o/r", "t", "Closes #581\nCloses #590", nil)
	if got.Held {
		t.Fatalf("Held = true (%s); a human-filed citation authorizes the PR", got.Reason)
	}
}

// TestEvaluateSelfAuthorization_RecognisesTheAIAuthorAccount covers the hive
// that files issues under a plain user account (project.ai_author) rather than
// the App bot: that login has no "[bot]" suffix and no Bot type, so only the
// configured identity can tell it apart from a maintainer.
func TestEvaluateSelfAuthorization_RecognisesTheAIAuthorAccount(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "hive-worker"}}}
	c := NewClientForTest(srv.start(t).URL, "o", nil, prTestLogger())

	if got := c.EvaluateSelfAuthorization(context.Background(), "o/r", "t", "Closes #581", nil); got.Held {
		t.Fatalf("Held before identity was configured; a plain login must read as human until told otherwise")
	}

	c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-worker"})
	if got := c.EvaluateSelfAuthorization(context.Background(), "o/r", "t", "Closes #581", nil); !got.Held {
		t.Fatal("Held = false after the identity named hive-worker as ours")
	}
}

// TestPRRequestWatcher_HoldsSelfAuthorizedPR is the end-to-end shape: the PR is
// still opened (the work is not thrown away), it carries "hold", and it carries
// a comment saying what would clear it.
func TestPRRequestWatcher_HoldsSelfAuthorizedPR(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot"}}}
	c := testClient(t, srv.start(t).URL)
	c.SetAppBotLogin(botLogin)
	// No hold-gated level: at L6 this gate is the only thing standing between a
	// self-proposed direction and a merged one.
	c.prHoldLabel = func(agent string) bool { return false }

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "decompose-phase-1", Base: "main",
		Title: "phase 1 of the decomposition", Body: "Closes #581", Agent: "quality",
	})
	if err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}

	c.ProcessPRRequestsOnce(context.Background())

	if applied := srv.applied(); len(applied) != 1 || applied[0] != "hold" {
		t.Fatalf("labels applied = %v, want [hold]", applied)
	}
	comments := srv.postedComments()
	if len(comments) != 1 {
		t.Fatalf("posted %d comments, want 1 explaining the hold", len(comments))
	}
	for _, want := range []string{"#581", HumanAckLabel, "5117"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("hold explanation does not mention %q:\n%s", want, comments[0])
		}
	}

	resultPath := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	var resp PRResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if !resp.OK || resp.Number != 583 {
		t.Fatalf("result = %+v, want the PR to have been opened", resp)
	}
	if !resp.SelfAuthorized {
		t.Error("self_authorized = false in the result; the agent cannot tell why its PR is held")
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("request file survived; the PR was opened, so the request is settled")
	}
}

func TestPRRequestWatcher_DefaultConfigStillHoldsSelfAuthorizedPR(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot"}}}
	c := testClient(t, srv.start(t).URL)
	c.SetAppBotLogin(botLogin)
	c.prHoldLabel = func(agent string) bool { return false }

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	if _, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "decompose-phase-1", Base: "main",
		Title: "phase 1 of the decomposition", Body: "Closes #581", Agent: "quality",
	}); err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if applied := srv.applied(); len(applied) != 1 || applied[0] != "hold" {
		t.Fatalf("labels applied = %v, want [hold] by default", applied)
	}
	if comments := srv.postedComments(); len(comments) != 1 || !strings.Contains(comments[0], SelfAuthorizationNoticeMarker) {
		t.Fatalf("comments = %v, want one marked #5117 notice", comments)
	}
}

func TestPRRequestWatcher_ConfigDisabledSkipsSelfAuthorizationHold(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot"}}}
	c := testClient(t, srv.start(t).URL)
	c.SetAppBotLogin(botLogin)
	c.SetSelfAuthorizationHoldEnabled(func(repo string) bool { return false })
	c.prHoldLabel = func(agent string) bool { return false }

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	if _, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "decompose-phase-1", Base: "main",
		Title: "phase 1 of the decomposition", Body: "Closes #581", Agent: "quality",
	}); err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if applied := srv.applied(); len(applied) != 0 {
		t.Fatalf("labels applied = %v, want no #5117 hold when disabled", applied)
	}
	if comments := srv.postedComments(); len(comments) != 0 {
		t.Fatalf("posted %d comments, want no #5117 notice when disabled", len(comments))
	}
}

func TestPRRequestWatcher_ConfigCallbackReceivesRepo(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot"}}}
	c := testClient(t, srv.start(t).URL)
	c.SetAppBotLogin(botLogin)
	c.SetSelfAuthorizationHoldEnabled(func(repo string) bool { return repo != "o/r" })
	c.prHoldLabel = func(agent string) bool { return false }

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	if _, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "decompose-phase-1", Base: "main",
		Title: "phase 1 of the decomposition", Body: "Closes #581", Agent: "quality",
	}); err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}

	c.ProcessPRRequestsOnce(context.Background())

	if applied := srv.applied(); len(applied) != 0 {
		t.Fatalf("labels applied = %v, want repo-specific disabled hold skipped", applied)
	}
}

// TestPRRequestWatcher_DoesNotHoldHumanBackedPR is the control. Without it the
// test above would also pass if the watcher simply held everything.
func TestPRRequestWatcher_DoesNotHoldHumanBackedPR(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "hanthor"}}}
	c := testClient(t, srv.start(t).URL)
	c.SetAppBotLogin(botLogin)
	c.prHoldLabel = func(agent string) bool { return false }

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	if _, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "fix", Base: "main", Title: "fix", Body: "Closes #581", Agent: "quality",
	}); err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if applied := srv.applied(); len(applied) != 0 {
		t.Fatalf("labels applied = %v, want none — a human filed the issue", applied)
	}
	if comments := srv.postedComments(); len(comments) != 0 {
		t.Fatalf("posted %d comments, want none", len(comments))
	}
}

// TestPRRequestWatcher_HoldGatedLevelSkipsTheLookup pins the short-circuit. At a
// hold-gated level the PR is held anyway, so spending API calls to discover a
// second reason to hold it buys nothing.
func TestPRRequestWatcher_HoldGatedLevelSkipsTheLookup(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot",
		Comments: []selfAuthComment{{Author: "someone"}}}}}
	c := testClient(t, srv.start(t).URL)
	c.SetAppBotLogin(botLogin)
	c.prHoldLabel = func(agent string) bool { return true }

	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })

	if _, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "fix", Base: "main", Title: "fix", Body: "Closes #581", Agent: "quality",
	}); err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if applied := srv.applied(); len(applied) != 1 || applied[0] != "hold" {
		t.Fatalf("labels applied = %v, want [hold] from the level alone", applied)
	}
	srv.mu.Lock()
	listCalls := srv.listCalls
	srv.mu.Unlock()
	if listCalls != 0 {
		t.Errorf("gate made %d comment lookups at a hold-gated level, want 0", listCalls)
	}
}
