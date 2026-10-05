package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

// ---------- admission: enumeration honours the reporter gate ----------

func reporterTrustIssues() []wireIssue {
	return []wireIssue{
		{Number: 1, Title: "maintainer asks, no label", User: wireUser{"maintainer"}, AuthorAssociation: "MEMBER",
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(4)},
		{Number: 2, Title: "stranger asks, no label", User: wireUser{"stranger"}, AuthorAssociation: "NONE",
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(3)},
		{Number: 3, Title: "stranger asks, triaged", User: wireUser{"stranger2"}, AuthorAssociation: "FIRST_TIMER",
			Labels: []wireLabel{{Name: "triage/accepted"}}, CreatedAt: hoursAgo(2)},
		{Number: 4, Title: "hive filed it", User: wireUser{"kubestellar-hive[bot]"}, AuthorAssociation: "NONE",
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(1)},
		{Number: 5, Title: "trusted login, no association", User: wireUser{"external-maintainer"}, AuthorAssociation: "NONE",
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(1)},
	}
}

func enumerateWithReporterTrust(t *testing.T, f config.IssueFilterConfig) *ActionableResult {
	t.Helper()
	org, repo := "testorg", "testrepo"
	mux := buildMux(t, org, repo, reporterTrustIssues(), nil)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(f)
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	return result
}

func enabledReporterTrust() config.IssueFilterConfig {
	on := true
	return config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{
		Enabled:       &on,
		TrustedLogins: []string{"external-maintainer"},
	}}
}

// TestEnumerateActionable_ReporterTrust pins the gate at THE choice point:
// a maintainer's unlabelled issue is actionable, a stranger's is not until
// triaged, and a stranger's triaged issue is. The bot-filed issue is not a
// "reporter" and is left to #5117 on the PR side.
func TestEnumerateActionable_ReporterTrust(t *testing.T) {
	result := enumerateWithReporterTrust(t, enabledReporterTrust())
	nums := actionableNumbers(result)
	if !nums[1] {
		t.Error("positive control failed: the MEMBER's unlabelled issue must be actionable")
	}
	if nums[2] {
		t.Error("the stranger's untriaged issue entered the actionable set — the reporter gate is not enforced")
	}
	if !nums[3] {
		t.Error("the stranger's issue carrying triage/accepted must be actionable")
	}
	if !nums[4] {
		t.Error("a hive/bot-filed issue is not judged by the reporter gate; #5117 owns it")
	}
	if !nums[5] {
		t.Error("an explicitly trusted login must be admitted whatever its association")
	}
	var triage int
	for _, r := range result.WorkBreakdownByRepo {
		triage += r.Issues.ReporterTriage
	}
	if triage != 1 {
		t.Errorf("reporter_triage = %d, want 1 (issue #2 awaiting triage)", triage)
	}
}

// TestEnumerateActionable_ReporterTrustOffUnchanged is the regression pin for
// every existing hive: with the block absent, all five issues are actionable
// and nothing is counted as awaiting triage.
func TestEnumerateActionable_ReporterTrustOffUnchanged(t *testing.T) {
	result := enumerateWithReporterTrust(t, config.IssueFilterConfig{})
	if got := result.Issues.Count; got != 5 {
		t.Errorf("Issues.Count = %d, want 5 — absent reporter_trust must change nothing", got)
	}
	for _, r := range result.WorkBreakdownByRepo {
		if r.Issues.ReporterTriage != 0 {
			t.Errorf("reporter_triage = %d with the gate off", r.Issues.ReporterTriage)
		}
	}
}

// TestEnumerateActionable_ReporterTrustComposesWithRequireLabels: the
// ordinary allow-list still applies to trusted reporters afterwards, so
// "everyone needs an approval label" remains expressible exactly as before.
func TestEnumerateActionable_ReporterTrustComposesWithRequireLabels(t *testing.T) {
	f := enabledReporterTrust()
	f.RequireLabels = []string{"triage/accepted"}
	result := enumerateWithReporterTrust(t, f)
	nums := actionableNumbers(result)
	if nums[1] {
		t.Error("the MEMBER's issue passed the reporter gate but lacks the required label; require_labels must still refuse it")
	}
	if !nums[3] {
		t.Error("the triaged stranger's issue satisfies both gates and must be actionable")
	}
}

type reporterTrustWaitHarness struct {
	t        *testing.T
	org      string
	repo     string
	issue    wireIssue
	comments []string
	labels   map[string]bool
}

func newReporterTrustWaitHarness(t *testing.T, issue wireIssue) *reporterTrustWaitHarness {
	t.Helper()
	h := &reporterTrustWaitHarness{
		t:      t,
		org:    "testorg",
		repo:   "testrepo",
		issue:  issue,
		labels: map[string]bool{},
	}
	for _, label := range issue.Labels {
		h.labels[label.Name] = true
	}
	return h
}

func (h *reporterTrustWaitHarness) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", h.org, h.repo), func(w http.ResponseWriter, r *http.Request) {
		h.issue.Labels = h.currentLabels()
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(h.t, []wireIssue{h.issue}))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls", h.org, h.repo), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/comments", h.org, h.repo, h.issue.Number), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			out := make([]map[string]string, 0, len(h.comments))
			for _, body := range h.comments {
				out = append(out, map[string]string{"body": body})
			}
			w.Write(mustMarshal(h.t, out))
		case http.MethodPost:
			var req struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				h.t.Fatalf("decode comment: %v", err)
			}
			h.comments = append(h.comments, req.Body)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"html_url":"https://example.test/comment"}`))
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/labels", h.org, h.repo, h.issue.Number), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var labels []string
		if err := json.NewDecoder(r.Body).Decode(&labels); err != nil {
			h.t.Fatalf("decode labels: %v", err)
		}
		for _, label := range labels {
			h.labels[label] = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/labels/", h.org, h.repo, h.issue.Number), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		label, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/%d/labels/", h.org, h.repo, h.issue.Number)))
		if err != nil {
			h.t.Fatalf("unescape label: %v", err)
		}
		delete(h.labels, label)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", h.org, h.repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"created"}`))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels/", h.org, h.repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		label, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/labels/", h.org, h.repo)))
		if h.labels[label] {
			w.Write([]byte(`{"name":"` + label + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"not found"}`))
	})
	return httptest.NewServer(mux)
}

func (h *reporterTrustWaitHarness) currentLabels() []wireLabel {
	out := make([]wireLabel, 0, len(h.labels))
	for label := range h.labels {
		out = append(out, wireLabel{Name: label})
	}
	return out
}

func TestEnumerateActionable_ReporterTrustWaitCommentAndLabel(t *testing.T) {
	h := newReporterTrustWaitHarness(t, wireIssue{
		Number: 101, Title: "stranger asks", User: wireUser{"stranger"}, AuthorAssociation: "NONE", CreatedAt: hoursAgo(1),
	})
	server := h.server()
	t.Cleanup(server.Close)
	c := newTestClient(t, server, h.org, []string{h.repo})
	c.SetIssueFilter(enabledReporterTrust())
	recs := captureAudit(c)

	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if result.Issues.Count != 0 {
		t.Fatalf("untrusted issue entered backlog before triage: %+v", result.Issues.Items)
	}
	if got := len(h.comments); got != 1 {
		t.Fatalf("comments = %d, want one reporter-trust wait comment", got)
	}
	if !strings.Contains(h.comments[0], reporterTrustWaitMarkerPrefix) || !strings.Contains(h.comments[0], "triage/accepted") || !strings.Contains(h.comments[0], "added-label=needs-triage") {
		t.Fatalf("comment body missing marker/label: %q", h.comments[0])
	}
	if !h.labels["needs-triage"] {
		t.Fatalf("awaiting-triage label not applied: %#v", h.labels)
	}
	if rec, ok := findAudit(*recs, AuditActionReporterTrustWaitNoticed); !ok {
		t.Fatalf("missing %s audit; records=%#v", AuditActionReporterTrustWaitNoticed, *recs)
	} else if rec.Repo != h.org+"/"+h.repo || rec.Target != h.issue.Number {
		t.Fatalf("notice audit target = %s#%d", rec.Repo, rec.Target)
	}
	if rec, ok := findAudit(*recs, AuditActionHiveLabelApplied); !ok {
		t.Fatalf("missing %s audit; records=%#v", AuditActionHiveLabelApplied, *recs)
	} else if !strings.Contains(rec.Detail, "reason=reporter_trust_wait") {
		t.Fatalf("label audit missing reporter-trust reason: %q", rec.Detail)
	}

	if _, err := c.EnumerateActionable(context.Background()); err != nil {
		t.Fatalf("second EnumerateActionable: %v", err)
	}
	if got := len(h.comments); got != 1 {
		t.Fatalf("second sweep duplicated comment: got %d comments", got)
	}
}

func TestEnumerateActionable_ReporterTrustWaitNoticeBudget(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issueCount := DefaultReporterTrustWaitMaxNotices + 1
	issues := make([]wireIssue, 0, issueCount)
	labels := map[int]map[string]bool{}
	comments := map[int]int{}
	for i := 1; i <= issueCount; i++ {
		issues = append(issues, wireIssue{
			Number: i, Title: fmt.Sprintf("stranger asks %d", i), User: wireUser{"stranger"},
			AuthorAssociation: "NONE", CreatedAt: hoursAgo(1),
		})
		labels[i] = map[string]bool{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		current := make([]wireIssue, 0, len(issues))
		for _, issue := range issues {
			for label := range labels[issue.Number] {
				issue.Labels = append(issue.Labels, wireLabel{Name: label})
			}
			current = append(current, issue)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, current))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	for _, issue := range issues {
		number := issue.Number
		mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/comments", org, repo, number), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method {
			case http.MethodGet:
				w.Write([]byte(`[]`))
			case http.MethodPost:
				comments[number]++
				w.WriteHeader(http.StatusCreated)
				w.Write([]byte(`{"html_url":"https://example.test/comment"}`))
			default:
				http.NotFound(w, r)
			}
		})
		mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/%d/labels", org, repo, number), func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			labels[number]["needs-triage"] = true
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
		})
	}
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"needs-triage"}`))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"not found"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	c.SetIssueFilter(enabledReporterTrust())

	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	var triage int
	for _, r := range result.WorkBreakdownByRepo {
		triage += r.Issues.ReporterTriage
	}
	if triage != issueCount {
		t.Fatalf("reporter_triage = %d, want %d", triage, issueCount)
	}
	var commentTotal, labelTotal int
	for i := 1; i <= issueCount; i++ {
		commentTotal += comments[i]
		if labels[i]["needs-triage"] {
			labelTotal++
		}
	}
	if commentTotal != DefaultReporterTrustWaitMaxNotices {
		t.Fatalf("comments posted = %d, want cap %d", commentTotal, DefaultReporterTrustWaitMaxNotices)
	}
	if labelTotal != DefaultReporterTrustWaitMaxNotices {
		t.Fatalf("labels applied = %d, want cap %d", labelTotal, DefaultReporterTrustWaitMaxNotices)
	}
}

func TestEnumerateActionable_ReporterTrustWaitClearsAfterTriage(t *testing.T) {
	h := newReporterTrustWaitHarness(t, wireIssue{
		Number: 102, Title: "stranger triaged", User: wireUser{"stranger"}, AuthorAssociation: "NONE",
		Labels: []wireLabel{{Name: "needs-triage"}, {Name: "triage/accepted"}}, CreatedAt: hoursAgo(1),
	})
	h.comments = []string{reporterTrustWaitComment(h.repo, "needs-triage", enabledReporterTrust())}
	server := h.server()
	t.Cleanup(server.Close)
	c := newTestClient(t, server, h.org, []string{h.repo})
	c.SetIssueFilter(enabledReporterTrust())
	recs := captureAudit(c)

	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if result.Issues.Count != 1 {
		t.Fatalf("triaged issue count = %d, want 1", result.Issues.Count)
	}
	if h.labels["needs-triage"] {
		t.Fatalf("awaiting label was not removed after triage: %#v", h.labels)
	}
	if got := len(h.comments); got != 1 {
		t.Fatalf("triaged issue should keep its existing wait comment only: %d", got)
	}
	if rec, ok := findAudit(*recs, AuditActionReporterTrustWaitCleared); !ok {
		t.Fatalf("missing %s audit; records=%#v", AuditActionReporterTrustWaitCleared, *recs)
	} else if rec.Repo != h.org+"/"+h.repo || rec.Target != h.issue.Number {
		t.Fatalf("clear audit target = %s#%d", rec.Repo, rec.Target)
	}
}

func TestEnumerateActionable_ReporterTrustWaitDoesNotClearHumanTriageLabel(t *testing.T) {
	h := newReporterTrustWaitHarness(t, wireIssue{
		Number: 104, Title: "stranger triaged elsewhere", User: wireUser{"stranger"}, AuthorAssociation: "NONE",
		Labels: []wireLabel{{Name: "needs-triage"}, {Name: "triage/accepted"}}, CreatedAt: hoursAgo(1),
	})
	h.comments = []string{reporterTrustWaitComment(h.repo, "", enabledReporterTrust())}
	server := h.server()
	t.Cleanup(server.Close)
	c := newTestClient(t, server, h.org, []string{h.repo})
	c.SetIssueFilter(enabledReporterTrust())

	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if result.Issues.Count != 1 {
		t.Fatalf("triaged issue count = %d, want 1", result.Issues.Count)
	}
	if !h.labels["needs-triage"] {
		t.Fatalf("human/bot-owned needs-triage label was removed: %#v", h.labels)
	}
}

func TestReporterTrustWaitMarkedAddedExact(t *testing.T) {
	comments := []string{reporterTrustWaitMarker("repo", "needs-triage-old")}
	if reporterTrustWaitMarkedAdded(comments, "needs-triage") {
		t.Fatal("prefix match must not prove Hive added the current waiting label")
	}
	if !reporterTrustWaitMarkedAdded(comments, "needs-triage-old") {
		t.Fatal("exact added-label marker was not recognized")
	}
}

func TestEnumerateActionable_ReporterTrustWaitTrustedReporterNoop(t *testing.T) {
	h := newReporterTrustWaitHarness(t, wireIssue{
		Number: 103, Title: "maintainer asks", User: wireUser{"maintainer"}, AuthorAssociation: "MEMBER", CreatedAt: hoursAgo(1),
	})
	server := h.server()
	t.Cleanup(server.Close)
	c := newTestClient(t, server, h.org, []string{h.repo})
	c.SetIssueFilter(enabledReporterTrust())

	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if result.Issues.Count != 1 {
		t.Fatalf("trusted issue count = %d, want 1", result.Issues.Count)
	}
	if len(h.comments) != 0 || h.labels["needs-triage"] {
		t.Fatalf("trusted reporter should not be marked: comments=%d labels=%#v", len(h.comments), h.labels)
	}
}

// ---------- the PR-side evaluator ----------

func reporterTrustTestClient(t *testing.T, srv *selfAuthServer, holdActive bool) *Client {
	t.Helper()
	c := testClient(t, srv.start(t).URL)
	c.SetAppBotLogin("kubestellar-hive[bot]")
	c.SetReporterTrustHoldEnabled(func(string) bool { return holdActive })
	trust := enabledReporterTrust().ReporterTrust
	c.SetReporterTrusted(trust.Trusted)
	return c
}

func TestEvaluateReporterTrust(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	cases := []struct {
		name     string
		issues   map[int]*selfAuthIssue
		body     string
		declared []int
		wantHeld bool
		wantWhy  string
	}{{
		name:     "a stranger asked: hold",
		issues:   map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}},
		body:     "Closes #581",
		wantHeld: true, wantWhy: "not a trusted reporter",
	}, {
		name:     "a maintainer asked: no hold",
		issues:   map[int]*selfAuthIssue{581: {Author: "maintainer", Association: "MEMBER"}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name:     "an explicitly trusted login with no association: no hold",
		issues:   map[int]*selfAuthIssue{581: {Author: "external-maintainer"}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name:     "association missing from the payload fails toward the hold",
		issues:   map[int]*selfAuthIssue{581: {Author: "somebody"}},
		body:     "Closes #581",
		wantHeld: true, wantWhy: "association unknown",
	}, {
		name:     "the hive filed it: not this gate's business",
		issues:   map[int]*selfAuthIssue{581: {Author: botLogin, AuthorType: "Bot", Association: "NONE"}},
		body:     "Closes #581",
		wantHeld: false,
	}, {
		name: "one stranger among maintainers still holds",
		issues: map[int]*selfAuthIssue{
			581: {Author: "maintainer", Association: "OWNER"},
			590: {Author: "stranger", Association: "FIRST_TIME_CONTRIBUTOR"},
		},
		body:     "Closes #581\nRefs #590",
		wantHeld: true,
	}, {
		name:     "the request's declared issue list counts",
		issues:   map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}},
		body:     "No references in the prose.",
		declared: []int{581},
		wantHeld: true,
	}, {
		name:     "no rationale cited decides nothing",
		issues:   map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}},
		body:     "Just a change.",
		wantHeld: false,
	}, {
		name:     "an unreadable issue decides nothing",
		issues:   map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE", Status: 500}},
		body:     "Closes #581",
		wantHeld: false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &selfAuthServer{issues: tc.issues}
			c := reporterTrustTestClient(t, srv, true)
			got := c.EvaluateReporterTrust(context.Background(), "o/r", "t", tc.body, tc.declared)
			if got.Held != tc.wantHeld {
				t.Fatalf("Held = %v, want %v (reason %q)", got.Held, tc.wantHeld, got.Reason)
			}
			if tc.wantHeld && tc.wantWhy != "" && !strings.Contains(got.Reason, tc.wantWhy) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tc.wantWhy)
			}
		})
	}
}

// ---------- the watcher applies it at every level ----------

func runReporterTrustWatcher(t *testing.T, c *Client) (reqPath string) {
	t.Helper()
	dir := t.TempDir()
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = "" })
	reqPath, err := WritePRRequest(dir, PRRequest{
		Repo: "o/r", Head: "barbie-theme", Base: "main",
		Title: "make the default dashboard theme Barbie", Body: "Closes #581", Agent: "quality",
	})
	if err != nil {
		t.Fatalf("WritePRRequest: %v", err)
	}
	c.ProcessPRRequestsOnce(context.Background())
	return reqPath
}

func TestPRRequestWatcher_HoldsUntrustedReporterPRAtL6(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	c := reporterTrustTestClient(t, srv, true)
	// L6: no level hold. The reporter gate is the only thing between a
	// stranger's request and an unattended merge.
	c.prHoldLabel = func(string) bool { return false }

	reqPath := runReporterTrustWatcher(t, c)

	if applied := srv.applied(); len(applied) != 1 || applied[0] != "hold" {
		t.Fatalf("labels applied = %v, want [hold]", applied)
	}
	comments := srv.postedComments()
	if len(comments) != 1 {
		t.Fatalf("posted %d comments, want 1 explaining the hold", len(comments))
	}
	for _, want := range []string{ReporterTrustNoticeMarker, "#581", "@stranger", "NONE", "9665"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("hold explanation does not mention %q:\n%s", want, comments[0])
		}
	}
	raw, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	var resp PRResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if !resp.OK || !resp.ReporterTrustHeld {
		t.Errorf("result = %+v, want OK with reporter_trust_held so the agent knows why", resp)
	}
	if resp.SelfAuthorized {
		t.Error("a human-filed issue is not a #5117 hold; the two gates must not be confused")
	}
}

func TestPRRequestWatcher_TrustedReporterPRNotHeld(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "maintainer", Association: "COLLABORATOR"}}}
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return false }
	runReporterTrustWatcher(t, c)
	if applied := srv.applied(); len(applied) != 0 {
		t.Fatalf("labels applied = %v, want none for a collaborator's request", applied)
	}
	if n := len(srv.postedComments()); n != 0 {
		t.Fatalf("posted %d comments, want none", n)
	}
}

func TestPRRequestWatcher_ReporterTrustOffDoesNothing(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	c := reporterTrustTestClient(t, srv, false)
	c.prHoldLabel = func(string) bool { return false }
	runReporterTrustWatcher(t, c)
	if applied := srv.applied(); len(applied) != 0 {
		t.Fatalf("labels applied = %v with the gate off, want none", applied)
	}
}

// At a hold-gated level the label was going on anyway; what matters is that
// the reporter-trust notice ALSO lands, because it is what stops the level
// hold release from lifting the label on promotion to L6.
func TestPRRequestWatcher_HoldGatedLevelStillPostsReporterTrustNotice(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return true }
	runReporterTrustWatcher(t, c)
	if applied := srv.applied(); len(applied) != 1 || applied[0] != "hold" {
		t.Fatalf("labels applied = %v, want [hold]", applied)
	}
	comments := srv.postedComments()
	var level, reporter bool
	for _, body := range comments {
		if _, ok := levelHoldAgentFromNotice(body); ok {
			level = true
		}
		if IsReporterTrustHoldNotice(body) {
			reporter = true
		}
	}
	if !level || !reporter {
		t.Fatalf("want both a level notice and a reporter-trust notice, got level=%v reporter=%v in %d comments", level, reporter, len(comments))
	}
}

// ---------- promotion to L6 must not release a reporter-trust hold ----------

func reporterHeldPR(number int) *gh.PullRequest {
	return &gh.PullRequest{
		Number: gh.Ptr(number),
		Title:  gh.Ptr("make the default dashboard theme Barbie"),
		Body:   gh.Ptr("Closes #581"),
		Labels: []*gh.Label{{Name: gh.Ptr("hold")}},
	}
}

func TestReleaseLevelHold_ReporterTrustNoticeBlocksRelease(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	// Seed the PR's comment thread with a level notice AND the reporter notice,
	// as the watcher leaves it at L5; the fake serves s.comments for any
	// number it has no issue for, attributed to the App bot.
	srv.comments = []string{levelHoldNotice("quality"), reporterTrustNotice(ReporterTrust{Held: true, Issue: 581, Repo: "o/r", Reporter: "stranger", Association: "NONE"})}
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return false } // promoted to L6: level no longer requires the hold

	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", reporterHeldPR(583))
	if err != nil {
		t.Fatalf("releaseLevelHoldIfEligible: %v", err)
	}
	if released || reason != "hold" {
		t.Fatalf("released=%v reason=%q; a reporter-trust hold is a human's to lift", released, reason)
	}
}

func TestReleaseLevelHold_ReEvaluatesAndPostsMissingReporterNotice(t *testing.T) {
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "stranger", Association: "NONE"}}}
	srv.comments = []string{levelHoldNotice("quality")} // the reporter notice never landed
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return false }

	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", reporterHeldPR(583))
	if err != nil {
		t.Fatalf("releaseLevelHoldIfEligible: %v", err)
	}
	if released || reason != "hold" {
		t.Fatalf("released=%v reason=%q; the hold must survive", released, reason)
	}
}

func TestReleaseLevelHold_TrustedReporterStillReleases(t *testing.T) {
	// Positive control: with a maintainer's rationale, the reporter gate does
	// not interfere and the level hold reaches its ordinary release checks.
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{581: {Author: "maintainer", Association: "OWNER"}}}
	srv.comments = []string{levelHoldNotice("quality")}
	c := reporterTrustTestClient(t, srv, true)
	c.prHoldLabel = func(string) bool { return false }

	_, reason, _ := c.releaseLevelHoldIfEligible(context.Background(), "o", "r", reporterHeldPR(583))
	if reason != "hold" {
		t.Fatalf("reason = %q; a trusted reporter's PR must not be held by the reporter gate", reason)
	}
}
