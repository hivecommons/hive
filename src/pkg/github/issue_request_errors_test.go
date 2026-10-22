package github

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
)

func ghStatusErr(status int) error {
	u, _ := url.Parse("https://api.github.example/repos/o/r/issues")
	return &gh.ErrorResponse{Response: &http.Response{StatusCode: status, Request: &http.Request{Method: "POST", URL: u}}, Message: http.StatusText(status)}
}

func TestIsPlaceholderRepoRef(t *testing.T) {
	for _, tc := range []struct {
		owner, name string
		want        bool
	}{
		{"owner", "repo", true},
		{"OWNER", "REPO", true},
		{"org", "repo", true},
		{"example", "anything", true},
		{"your-org", "service", true},
		{"hivecommons", "your-repo", true},
		{"hivecommons", "hive", false},
		{"kubestellar", "console", false},
		{"hanthor", "reilly.asia", false},
	} {
		if got := isPlaceholderRepoRef(tc.owner, tc.name); got != tc.want {
			t.Errorf("isPlaceholderRepoRef(%q, %q) = %v, want %v", tc.owner, tc.name, got, tc.want)
		}
	}
}

func TestIssueRequestFailureClassification(t *testing.T) {
	rateLimited := &gh.RateLimitError{Response: &http.Response{StatusCode: http.StatusForbidden}, Message: "rate limited"}
	var netErr net.Error = &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
		definite  bool
	}{
		{"nil", nil, false, false},
		{"404", ghStatusErr(http.StatusNotFound), true, true},
		{"410", ghStatusErr(http.StatusGone), true, true},
		{"422", ghStatusErr(http.StatusUnprocessableEntity), true, true},
		{"401", ghStatusErr(http.StatusUnauthorized), true, true},
		{"403 permission", ghStatusErr(http.StatusForbidden), true, true},
		{"403 rate limit", rateLimited, false, false},
		{"429", ghStatusErr(http.StatusTooManyRequests), false, false},
		{"502", ghStatusErr(http.StatusBadGateway), false, false},
		{"network", netErr, false, false},
		{"needs reconciliation", fmt.Errorf("creating: %w", effects.ErrNeedsReconciliation), false, false},
		{"relay repo refusal", permanentIssueRequestErrorf("refusing repo"), true, false},
		{"structural repo refusal", fmt.Errorf("CreateIssue: %w", validateRepoRef("<org>", "<repo>")), true, false},
		{"other local error", errors.New("boom"), false, false},
	} {
		if got := issueRequestFailureIsPermanent(tc.err); got != tc.permanent {
			t.Errorf("%s: issueRequestFailureIsPermanent = %v, want %v", tc.name, got, tc.permanent)
		}
		if got := isDefiniteIssueRefusal(tc.err); got != tc.definite {
			t.Errorf("%s: isDefiniteIssueRefusal = %v, want %v", tc.name, got, tc.definite)
		}
	}
}

func TestValidateIssueRequestRepo(t *testing.T) {
	c := issueTestClient(t, "http://127.0.0.1:0")
	if err := c.validateIssueRequestRepo("o/r"); err != nil {
		t.Fatalf("configured repo refused: %v", err)
	}
	if err := c.validateIssueRequestRepo("r"); err != nil {
		t.Fatalf("bare configured repo refused: %v", err)
	}
	for _, repo := range []string{"owner/repo", "o/other", "<org>/<repo>", "o/r/x", "O/R/", ""} {
		err := c.validateIssueRequestRepo(repo)
		var perm *permanentIssueRequestError
		if !errors.As(err, &perm) {
			t.Errorf("repo %q: want permanent refusal, got %v", repo, err)
		}
	}
	if err := c.validateIssueRequestRepo("owner/repo"); err == nil || !strings.Contains(err.Error(), "placeholder") || !strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("placeholder refusal must name the cause and the safe test path, got %v", err)
	}
	if err := c.validateIssueRequestRepo("o/other"); err == nil || !strings.Contains(err.Error(), "configured repository set") {
		t.Errorf("out-of-scope refusal must say so, got %v", err)
	}

	// A hive without a repo list cannot judge scope but still refuses
	// placeholders; a hive that literally configured a placeholder-looking
	// name owns it.
	c.SetRepos(nil)
	if err := c.validateIssueRequestRepo("o/anything"); err != nil {
		t.Errorf("no configured set: scope must not be enforced, got %v", err)
	}
	if err := c.validateIssueRequestRepo("owner/repo"); err == nil {
		t.Error("no configured set: placeholder must still be refused")
	}
	c.SetRepos([]string{"org/repo"})
	if err := c.validateIssueRequestRepo("org/repo"); err != nil {
		t.Errorf("a configured repo is real whatever it is called, got %v", err)
	}
}

// The incident shape: a smoke-test request naming the placeholder owner/repo
// is quarantined on the first tick — .failed, result written, no GitHub
// call, no retry.
func TestIssueRequestWatcher_QuarantinesPlaceholderRepoImmediately(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "issue", Repo: "owner/repo", Agent: "adjudicator", Title: "t", Body: "b", Labels: []string{"agent/testagent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	clock := func() time.Time { return now }
	c.processIssueRequests(context.Background(), clock)

	if _, err := os.Stat(reqPath + ".failed"); err != nil {
		t.Fatalf("expected .failed quarantine: %v", err)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("original request should be renamed away")
	}
	resp := readIssueResultFile(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, "placeholder") {
		t.Errorf("result = %+v, want a placeholder refusal", resp)
	}
	if calls.Load() != 0 {
		t.Errorf("placeholder repo must cost zero GitHub calls, got %d", calls.Load())
	}
	// Nothing left to retry: later ticks are no-ops.
	now = now.Add(issueRetryMax)
	c.processIssueRequests(context.Background(), clock)
	if calls.Load() != 0 {
		t.Errorf("quarantined request must not be retried, got %d calls", calls.Load())
	}
}

func TestIssueRequestWatcher_QuarantinesRepoOutsideConfiguredSet(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/elsewhere", Number: 4, Agent: "scanner", Body: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath + ".failed"); err != nil {
		t.Fatalf("expected .failed quarantine: %v", err)
	}
	if resp := readIssueResultFile(t, reqPath); resp.OK || !strings.Contains(resp.Error, "configured repository set") {
		t.Errorf("result = %+v, want an out-of-scope refusal", resp)
	}
	if calls.Load() != 0 {
		t.Errorf("out-of-scope repo must cost zero GitHub calls, got %d", calls.Load())
	}
}

// A definite GitHub refusal (here 404 on the create) is permanent: one
// attempt, .failed, never retried — not 24 hours of backoff.
func TestIssueRequestWatcher_QuarantinesDefiniteRefusalImmediately(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			fmt.Fprint(w, `{"name":"x"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			creates.Add(1)
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "[scanner] gone", Body: "x", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	clock := func() time.Time { return now }
	c.processIssueRequests(context.Background(), clock)
	if _, err := os.Stat(reqPath + ".failed"); err != nil {
		t.Fatalf("expected immediate .failed quarantine on 404: %v", err)
	}
	if resp := readIssueResultFile(t, reqPath); resp.OK || !strings.Contains(resp.Error, "404") {
		t.Errorf("result = %+v, want the 404", resp)
	}
	now = now.Add(issueRetryMax)
	c.processIssueRequests(context.Background(), clock)
	if creates.Load() != 1 {
		t.Errorf("a definite refusal must be attempted exactly once, got %d", creates.Load())
	}
}

// A create that keeps failing with the identical transient error gives up
// after issueRequestMaxIdenticalFailures attempts, well inside the 24h
// horizon, and names the quarantined path.
func TestIssueRequestWatcher_GivesUpAfterIdenticalFailures(t *testing.T) {
	created := 0
	fails := 1000
	srv := newIssueMockServer(t, "", &created, &fails, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "[scanner] same answer", Body: "x", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	clock := func() time.Time { return now }
	for i := 1; i < issueRequestMaxIdenticalFailures; i++ {
		c.processIssueRequests(context.Background(), clock)
		if _, err := os.Stat(reqPath); err != nil {
			t.Fatalf("attempt %d: request must still be queued: %v", i, err)
		}
		now = now.Add(issueRetryMax + time.Second)
	}
	c.processIssueRequests(context.Background(), clock)
	if _, err := os.Stat(reqPath + ".failed"); err != nil {
		t.Fatalf("expected .failed after %d identical failures: %v", issueRequestMaxIdenticalFailures, err)
	}
	if 1000-fails != issueRequestMaxIdenticalFailures {
		t.Errorf("attempts = %d, want %d", 1000-fails, issueRequestMaxIdenticalFailures)
	}
	if created != 0 {
		t.Errorf("nothing should have been created")
	}
}

// A changing error string resets the identical-failure run: distinct
// transient answers keep retrying up to the age horizon, as before.
func TestIssueNoteFailure_IdenticalRunResetsOnDifferentError(t *testing.T) {
	c := issueTestClient(t, "http://127.0.0.1:0")
	now := time.Now()
	clock := func() time.Time { return now }
	for i := 0; i < issueRequestMaxIdenticalFailures*2; i++ {
		if why := c.issueNoteFailure("p", "issue", clock, fmt.Errorf("attempt %d failed", i)); why != "" {
			t.Fatalf("distinct errors must not trip the identical-failure horizon: %q", why)
		}
	}
	// Other kinds keep the 24h horizon only.
	for i := 0; i < issueRequestMaxIdenticalFailures*2; i++ {
		if why := c.issueNoteFailure("q", "comment", clock, errors.New("same")); why != "" {
			t.Fatalf("a comment must not trip the create-only identical-failure horizon: %q", why)
		}
	}
	recon := fmt.Errorf("creating: %w", effects.ErrNeedsReconciliation)
	for i := 1; i < issueReconcileMaxAttempts; i++ {
		if why := c.issueNoteFailure("r", "issue", clock, recon); why != "" {
			t.Fatalf("attempt %d: too early to give up: %q", i, why)
		}
	}
	if why := c.issueNoteFailure("r", "issue", clock, recon); !strings.Contains(why, "reconciled") {
		t.Fatalf("want the reconciliation give-up, got %q", why)
	}
}

func clockedTestBoundary(t *testing.T, now func() time.Time) *mutation.Boundary {
	t.Helper()
	dir := t.TempDir()
	ledger, err := mutation.OpenLedger(filepath.Join(dir, "claims.json"), replayTestMaxWriters)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	return &mutation.Boundary{
		Executor: mutation.Executor{Ledger: ledger, Journal: journal, Mode: proof.ModeEnforce, Now: now},
		Holder:   "test",
	}
}

// An ambiguous first failure (502) leaves the journal Unknown. The next
// attempt must reconcile it against the repo's open issues — none, so
// NotApplied — and then create, rather than failing with the journal's
// needs-reconciliation refusal forever.
func TestCreateIssue_ReconcilesUnknownJournalEntryThenCreates(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			fmt.Fprint(w, `{"name":"x"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			if creates.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"number":42,"id":4242,"html_url":"https://github.example/o/r/issues/42"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetMutationBoundary(replayTestBoundary(t))
	req := CreateIssueRequest{Repo: "o/r", Title: "[scanner] ambiguous once", Body: "x", Origin: "scanner"}

	if _, err := c.CreateIssue(context.Background(), req); err == nil {
		t.Fatal("first create must surface the 502")
	}
	res, err := c.CreateIssue(context.Background(), req)
	if err != nil {
		t.Fatalf("second create must reconcile and proceed, got %v", err)
	}
	if res.Number != 42 || res.AlreadyExisted {
		t.Fatalf("second create = %+v, want fresh #42", res)
	}
	if creates.Load() != 2 {
		t.Errorf("POSTs = %d, want 2", creates.Load())
	}
}

// Reconciliation finds the earlier ambiguous attempt DID create the issue:
// the journal records it Applied and the create replays as the existing
// issue without a second POST.
func TestCreateIssue_ReconcilesUnknownJournalEntryToExistingIssue(t *testing.T) {
	var creates, lists atomic.Int32
	const title = "[scanner] ambiguous but landed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			fmt.Fprint(w, `{"name":"x"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			// The pre-create dedupe scan sees nothing (twice); only the
			// reconciliation lookup, the third list, sees the landed issue.
			if lists.Add(1) < 3 {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprintf(w, `[{"number":7,"title":%q,"html_url":"https://github.example/o/r/issues/7","user":{"login":"someone"}}]`, title)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			creates.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetMutationBoundary(replayTestBoundary(t))
	req := CreateIssueRequest{Repo: "o/r", Title: title, Body: "x", Origin: "scanner"}

	if _, err := c.CreateIssue(context.Background(), req); err == nil {
		t.Fatal("first create must surface the 502")
	}
	res, err := c.CreateIssue(context.Background(), req)
	if err != nil {
		t.Fatalf("second create must reconcile to the landed issue, got %v", err)
	}
	if res.Number != 7 || !res.AlreadyExisted {
		t.Fatalf("second create = %+v, want replay of #7", res)
	}
	if creates.Load() != 1 {
		t.Errorf("POSTs = %d, want only the ambiguous first", creates.Load())
	}
}

// When the reconciliation lookup itself fails, the Unknown entry stands
// (needs-reconciliation keeps surfacing, so the watcher can count it) until
// it is older than issueCreateUnknownTTL; then it expires and the create
// proceeds.
func TestCreateIssue_ExpiresUnreconcilableUnknownEntryAfterTTL(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			fmt.Fprint(w, `{"name":"x"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			// A terminal lookup failure: the dedupe scan is skipped and
			// reconciliation has nothing authoritative to go on.
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			if creates.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"number":43,"id":4343,"html_url":"https://github.example/o/r/issues/43"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := issueTestClient(t, srv.URL)
	c.SetMutationBoundary(clockedTestBoundary(t, func() time.Time { return now }))
	req := CreateIssueRequest{Repo: "o/r", Title: "[scanner] unreconcilable", Body: "x", Origin: "scanner"}

	if _, err := c.CreateIssue(context.Background(), req); err == nil {
		t.Fatal("first create must surface the 502")
	}
	now = now.Add(issueCreateUnknownTTL / 2)
	_, err := c.CreateIssue(context.Background(), req)
	if !errors.Is(err, effects.ErrNeedsReconciliation) {
		t.Fatalf("inside the TTL the journal refusal must stand, got %v", err)
	}
	if !strings.Contains(err.Error(), "reconciliation lookup failed") {
		t.Errorf("error must explain why reconciliation could not run: %v", err)
	}
	now = now.Add(issueCreateUnknownTTL)
	res, err := c.CreateIssue(context.Background(), req)
	if err != nil {
		t.Fatalf("past the TTL the entry must expire and the create proceed, got %v", err)
	}
	if res.Number != 43 {
		t.Fatalf("create = %+v, want fresh #43", res)
	}
	if creates.Load() != 2 {
		t.Errorf("POSTs = %d, want 2", creates.Load())
	}
}

// Two request files from two agents with identical content derive one
// logical operation (content dedupe is the feature). An ambiguous failure on
// the first must not fence the second forever: the second reconciles and
// creates, the first then replays as the created issue. One POST succeeds,
// both requests are consumed.
func TestIssueRequestWatcher_IdenticalRequestsDoNotDeadlockOnUnknownOp(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			fmt.Fprint(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			fmt.Fprint(w, `{"name":"x"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			if creates.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"number":11,"id":1111,"html_url":"https://github.example/o/r/issues/11"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetMutationBoundary(replayTestBoundary(t))
	dir := withIssueDir(t)

	pathA, err := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "[scanner] shared finding", Body: "same body", Agent: "adjudicator"})
	if err != nil {
		t.Fatal(err)
	}
	pathB, err := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "[scanner] shared finding", Body: "same body", Agent: "ci-maintainer"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	clock := func() time.Time { return now }
	for i := 0; i < 3; i++ {
		c.processIssueRequests(context.Background(), clock)
		now = now.Add(issueRetryMax + time.Second)
	}
	for _, p := range []string{pathA, pathB} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should have been consumed", filepath.Base(p))
		}
		if _, err := os.Stat(p + ".failed"); err == nil {
			t.Errorf("%s must not be quarantined", filepath.Base(p))
		}
		if resp := readIssueResultFile(t, p); !resp.OK || resp.Number != 11 {
			t.Errorf("%s result = %+v, want ok #11", filepath.Base(p), resp)
		}
	}
	if creates.Load() != 2 {
		t.Errorf("POSTs = %d, want the ambiguous one plus one real create", creates.Load())
	}
}
