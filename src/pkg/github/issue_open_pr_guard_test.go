package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// openPRGuardServer fakes the GitHub endpoints the open-PR claim guard and
// the `claim` relay request touch, recording the calls they make.
type openPRGuardServer struct {
	mu          sync.Mutex
	pulls       string
	pullLists   int
	labelExists bool
	labelsMade  int
	issueLabels map[string][]string
	removed     []string
}

func (f *openPRGuardServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			f.pullLists++
			_, _ = io.WriteString(w, f.pulls)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/labels/"+HasOpenPRLabel:
			if !f.labelExists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprintf(w, `{"name":%q}`, HasOpenPRLabel)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/labels":
			f.labelsMade++
			f.labelExists = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q}`, HasOpenPRLabel)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/") && strings.HasSuffix(r.URL.Path, "/labels"):
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			if f.issueLabels == nil {
				f.issueLabels = map[string][]string{}
			}
			f.issueLabels[r.URL.Path] = append(f.issueLabels[r.URL.Path], labels...)
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/") && strings.Contains(r.URL.Path, "/labels/"):
			f.removed = append(f.removed, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/"):
			_, _ = io.WriteString(w, `{"number":42,"labels":[{"name":"bug"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func openPRGuardPulls(prs ...string) string {
	return "[" + strings.Join(prs, ",") + "]"
}

func openPRGuardPull(number int, title, body string) string {
	return fmt.Sprintf(`{"number":%d,"state":"open","title":%q,"body":%q,"html_url":"https://github.com/o/r/pull/%d","user":{"login":"alice"},"head":{"sha":"abc"}}`,
		number, title, body, number)
}

// hivecommons/hive#14804: #14766 had open PR #14775 ("Fixes #14766") when it
// was claimed again. The guard reads open PRs live and refuses the claim when
// one links the issue with a closing keyword.
func TestIssueOpenPRClaimBlockReason(t *testing.T) {
	for _, tc := range []struct {
		name      string
		off       bool
		pulls     string
		want      string
		wantLabel bool
	}{
		{name: "Fixes in body", pulls: openPRGuardPulls(openPRGuardPull(7, "fix it", "Fixes #42")), want: ClaimBlockReasonOpenPRPrefix + "7", wantLabel: true},
		{name: "closes in title, any case", pulls: openPRGuardPulls(openPRGuardPull(8, "CLOSES #42: fix it", "")), want: ClaimBlockReasonOpenPRPrefix + "8", wantLabel: true},
		{name: "Resolves in body", pulls: openPRGuardPulls(openPRGuardPull(9, "fix", "This change. Resolves #42")), want: ClaimBlockReasonOpenPRPrefix + "9", wantLabel: true},
		{name: "earliest of two open PRs", pulls: openPRGuardPulls(openPRGuardPull(12, "fix", "Fixes #42"), openPRGuardPull(10, "fix", "Fixes #42")), want: ClaimBlockReasonOpenPRPrefix + "10", wantLabel: true},
		{name: "non-closing reference admits", pulls: openPRGuardPulls(openPRGuardPull(7, "fix", "Refs #42")), want: ""},
		{name: "other issue admits", pulls: openPRGuardPulls(openPRGuardPull(7, "fix", "Fixes #43")), want: ""},
		{name: "other repository admits", pulls: openPRGuardPulls(openPRGuardPull(7, "fix", "Fixes x/y#42")), want: ""},
		{name: "no open PRs admits", pulls: openPRGuardPulls(), want: ""},
		{name: "setting off admits", off: true, pulls: openPRGuardPulls(openPRGuardPull(7, "fix", "Fixes #42")), want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &openPRGuardServer{pulls: tc.pulls}
			srv := httptest.NewServer(fake.handler(t))
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.SetSkipIssuesWithOpenPR(!tc.off)

			if got := c.IssueOpenPRClaimBlockReason(context.Background(), "o/r", 42); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
			// A second refusal on the same issue neither relists nor relabels.
			if got := c.IssueOpenPRClaimBlockReason(context.Background(), "o/r", 42); got != tc.want {
				t.Fatalf("second reason = %q, want %q", got, tc.want)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			wantLists := 1
			if tc.off {
				wantLists = 0
			}
			if fake.pullLists != wantLists {
				t.Fatalf("open-PR listings = %d, want %d", fake.pullLists, wantLists)
			}
			labels := fake.issueLabels["/repos/o/r/issues/42/labels"]
			if tc.wantLabel {
				if len(labels) != 1 || labels[0] != HasOpenPRLabel || fake.labelsMade != 1 {
					t.Fatalf("issue labels %v, labels created %d; want %s once, created once", labels, fake.labelsMade, HasOpenPRLabel)
				}
			} else if len(labels) != 0 || fake.labelsMade != 0 {
				t.Fatalf("admitted issue was labelled: %v (created %d)", labels, fake.labelsMade)
			}
		})
	}
}

// One open-PR listing answers every issue in the repo for one sweep; a
// listing older than the sweep window is refreshed so a newly opened PR is
// seen.
func TestIssueOpenPRClaimListsOncePerSweep(t *testing.T) {
	fake := &openPRGuardServer{pulls: openPRGuardPulls(), labelExists: true}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetSkipIssuesWithOpenPR(true)
	now := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	c.openLinkedPRCache.now = func() time.Time { return now }

	for _, n := range []int{40, 41, 42} {
		if _, ok := c.IssueOpenPRClaim(context.Background(), "o/r", n); ok {
			t.Fatalf("#%d claimed with no open PRs", n)
		}
	}
	fake.mu.Lock()
	lists := fake.pullLists
	fake.pulls = openPRGuardPulls(openPRGuardPull(14775, "fix", "Fixes #42"))
	fake.mu.Unlock()
	if lists != 1 {
		t.Fatalf("listings for three issues = %d, want 1", lists)
	}

	now = now.Add(openLinkedPRCacheTTL)
	claim, ok := c.IssueOpenPRClaim(context.Background(), "o/r", 42)
	if !ok || claim.PRNumber != 14775 || claim.PRState != PRStateOpen || claim.PRAuthor != "alice" {
		t.Fatalf("after the sweep window: claim %+v, %v; want open PR #14775", claim, ok)
	}
}

// Once the linked PR merges or closes, the next admission of an issue the
// guard labelled clears HasOpenPRLabel instead of leaving it stale; issues
// the guard never labelled are not touched, and a later refusal relabels.
func TestIssueOpenPRClaimClearsLabelOnReadmit(t *testing.T) {
	fake := &openPRGuardServer{pulls: openPRGuardPulls(openPRGuardPull(14775, "fix", "Fixes #42")), labelExists: true}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetSkipIssuesWithOpenPR(true)
	now := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	c.openLinkedPRCache.now = func() time.Time { return now }

	if _, ok := c.IssueOpenPRClaim(context.Background(), "o/r", 42); !ok {
		t.Fatal("open PR #14775 did not refuse #42")
	}
	if _, ok := c.IssueOpenPRClaim(context.Background(), "o/r", 41); ok {
		t.Fatal("#41 refused with no linked PR")
	}

	fake.mu.Lock()
	fake.pulls = openPRGuardPulls()
	fake.mu.Unlock()
	now = now.Add(openLinkedPRCacheTTL)
	for i := 0; i < 2; i++ {
		if _, ok := c.IssueOpenPRClaim(context.Background(), "o/r", 42); ok {
			t.Fatalf("pass %d: #42 still refused after PR closed", i)
		}
	}
	fake.mu.Lock()
	removed := append([]string(nil), fake.removed...)
	fake.mu.Unlock()
	want := []string{"/repos/o/r/issues/42/labels/" + HasOpenPRLabel}
	if len(removed) != 1 || removed[0] != want[0] {
		t.Fatalf("label removals = %v, want %v once (never for #41, not twice for #42)", removed, want)
	}

	fake.mu.Lock()
	fake.pulls = openPRGuardPulls(openPRGuardPull(14790, "fix", "Fixes #42"))
	fake.mu.Unlock()
	now = now.Add(openLinkedPRCacheTTL)
	if _, ok := c.IssueOpenPRClaim(context.Background(), "o/r", 42); !ok {
		t.Fatal("new open PR #14790 did not refuse #42")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if labels := fake.issueLabels["/repos/o/r/issues/42/labels"]; len(labels) != 2 {
		t.Fatalf("issue labels %v, want %s applied twice (once per refusal episode)", labels, HasOpenPRLabel)
	}
}

// A failed listing never blocks a claim: the ledger's eval-cycle scan still
// covers the issue.
func TestIssueOpenPRClaimFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetSkipIssuesWithOpenPR(true)
	if got := c.IssueOpenPRClaimBlockReason(context.Background(), "o/r", 42); got != "" {
		t.Fatalf("listing failure refused the claim: %q", got)
	}
}

// The `claim` relay request on an issue an open PR already closes is denied
// with the PR number and writes no ownership label.
func TestIssueRequestWatcher_ClaimRefusesIssueWithOpenPR(t *testing.T) {
	fake := &openPRGuardServer{pulls: openPRGuardPulls(openPRGuardPull(14775, "fix", "Fixes #42"))}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetSkipIssuesWithOpenPR(true)
	dir := withIssueDir(t)
	path, err := WriteIssueRequest(dir, IssueRequest{Kind: "claim", Repo: "o/r", Number: 42, Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	res := readIssueResultFile(t, path)
	if res.OK || !strings.Contains(res.Error, ClaimBlockReasonOpenPRPrefix+"14775") {
		t.Fatalf("claim on issue with open PR: result %+v", res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, l := range fake.issueLabels["/repos/o/r/issues/42/labels"] {
		if l != HasOpenPRLabel {
			t.Fatalf("refused claim wrote label %q", l)
		}
	}
}
