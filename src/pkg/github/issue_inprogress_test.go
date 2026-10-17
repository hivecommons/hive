package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/issueclaim"
)

// Tests for the hive/in-progress start signal: the label and marker a
// `claim` request produces, the PR-open removal, and the enumeration sweep
// that withholds live marks and clears lapsed ones.

var inProgressNow = time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)

func enableInProgress(c *Client, hive string, ttl time.Duration) {
	c.appBotLogin = claimBotLogin
	c.SetInProgress(func() InProgressSetting { return InProgressSetting{Enabled: true, HiveID: hive, TTL: ttl} })
}

// inProgressRelayServer fakes the issue endpoints a claim/PR request touches
// and records every label and comment write.
type inProgressRelayServer struct {
	mu       sync.Mutex
	labels   [][]string
	removed  []string
	comments []string
	srv      *httptest.Server
}

func newInProgressRelayServer(t *testing.T) *inProgressRelayServer {
	t.Helper()
	s := &inProgressRelayServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/42":
			_, _ = io.WriteString(w, `{"number":42,"labels":[],"updated_at":"2026-10-14T14:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/43":
			// 43 already carries a claim's labels and the retired mark.
			_, _ = io.WriteString(w, `{"number":43,"labels":[{"name":"stage/030-pr-in-progress"},{"name":"hive/in-progress"},{"name":"bug"}],"updated_at":"2026-10-14T14:00:00Z"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.labels = append(s.labels, body)
			s.mu.Unlock()
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/labels/"):
			s.mu.Lock()
			s.removed = append(s.removed, r.URL.Path)
			s.mu.Unlock()
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(raw, &body)
			s.mu.Lock()
			s.comments = append(s.comments, body.Body)
			s.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *inProgressRelayServer) snapshot() (labels [][]string, comments []string, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.labels...), append([]string(nil), s.comments...), append([]string(nil), s.removed...)
}

func TestInProgressCommentBody_IsAParsableAgentClaimMarker(t *testing.T) {
	started := inProgressNow
	expires := started.Add(2 * time.Hour)
	body := InProgressCommentBody("scanner", "hive-a", started, expires)

	want := "<!-- hive:claim who=scanner kind=agent hive=hive-a at=2026-10-14T15:00:00Z until=2026-10-14T17:00:00Z -->"
	if !strings.HasPrefix(body, want) {
		t.Fatalf("marker line = %q, want prefix %q", strings.SplitN(body, "\n", 2)[0], want)
	}
	for _, s := range []string{"`scanner`", "hive hive-a", "started work on this issue at 2026-10-14T15:00:00Z", "lapses on its own at 2026-10-14T17:00:00Z"} {
		if !strings.Contains(body, s) {
			t.Errorf("human line missing %q:\n%s", s, body)
		}
	}
	claim, ok := issueclaim.ParseMarker(body)
	if !ok || claim.Identity != "scanner" || !claim.StartedAt.Equal(started) || !claim.ExpiresAt.Equal(expires) {
		t.Fatalf("issueclaim did not parse the marker: %+v ok=%v", claim, ok)
	}
	if !claim.Live(started.Add(time.Hour)) || claim.Live(expires) {
		t.Fatal("liveness window wrong")
	}
	// The ledger's own comment carries the same at= so both paths agree.
	ledger := claims.ClaimComment(claims.Claim{Holder: "scanner", Kind: claims.KindAgent, Hive: "hive-a", ClaimedAt: started, ExpiresAt: expires})
	if !strings.HasPrefix(ledger, want) {
		t.Fatalf("ledger ClaimComment marker drifted from the start mark:\n%s", ledger)
	}
}

// Disabled (no setter): a claim request is byte-for-byte the old behaviour —
// one ownership label, no comment.
func TestIssueRequestWatcher_ClaimWithoutInProgressIsUnchanged(t *testing.T) {
	fake := newInProgressRelayServer(t)
	c := issueTestClient(t, fake.srv.URL)
	withIssueDir(t)
	if _, err := WriteIssueRequest(issueRequestDirForTest, IssueRequest{Kind: "claim", Repo: "o/r", Number: 42, Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	labels, comments, _ := fake.snapshot()
	if len(labels) != 1 || len(labels[0]) != 1 || labels[0][0] != "hive/claimed-by-scanner" {
		t.Fatalf("labels = %v, want only the ownership label", labels)
	}
	if len(comments) != 0 {
		t.Fatalf("no comment expected with in-progress off, got %v", comments)
	}
}

// Enabled with governor.claims off: the claim request applies both labels in
// ONE call and posts the start mark itself.
func TestIssueRequestWatcher_ClaimAppliesInProgressLabelAndStartMark(t *testing.T) {
	fake := newInProgressRelayServer(t)
	c := issueTestClient(t, fake.srv.URL)
	enableInProgress(c, "hive-a", 2*time.Hour)
	withIssueDir(t)
	reqPath, err := WriteIssueRequest(issueRequestDirForTest, IssueRequest{Kind: "claim", Repo: "o/r", Number: 42, Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.processIssueRequests(context.Background(), func() time.Time { return inProgressNow })

	labels, comments, _ := fake.snapshot()
	if len(labels) != 1 {
		t.Fatalf("expected one label call, got %d: %v", len(labels), labels)
	}
	if strings.Join(labels[0], ",") != InProgressLabel+",hive/claimed-by-scanner" {
		t.Fatalf("labels = %v", labels[0])
	}
	if len(comments) != 1 {
		t.Fatalf("expected one start-mark comment, got %d", len(comments))
	}
	if !strings.Contains(comments[0], "who=scanner kind=agent hive=hive-a at=2026-10-14T15:00:00Z until=2026-10-14T17:00:00Z") {
		t.Fatalf("start mark missing fields:\n%s", comments[0])
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("claim request should be consumed on success")
	}
}

// Enabled with governor.claims ON: the ledger posts the 🔒 comment (now with
// at=), so the relay must not post a second marker. The label still goes on.
func TestIssueRequestWatcher_ClaimLeavesCommentToLedgerWhenClaimsOn(t *testing.T) {
	fake := newInProgressRelayServer(t)
	c := issueTestClient(t, fake.srv.URL)
	enableInProgress(c, "hive-a", 2*time.Hour)
	c.SetIssueClaims(func() (bool, time.Duration) { return true, time.Hour })
	withIssueDir(t)
	if _, err := WriteIssueRequest(issueRequestDirForTest, IssueRequest{Kind: "claim", Repo: "o/r", Number: 42, Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	labels, comments, _ := fake.snapshot()
	if len(labels) != 1 || !containsString(labels[0], InProgressLabel) {
		t.Fatalf("labels = %v, want %s present", labels, InProgressLabel)
	}
	if len(comments) != 0 {
		t.Fatalf("relay must not duplicate the ledger comment, got %v", comments)
	}
}

func TestClearInProgressForPR(t *testing.T) {
	fake := newInProgressRelayServer(t)
	c := issueTestClient(t, fake.srv.URL)
	// Off: nothing is removed.
	c.clearInProgressForPR(context.Background(), "o/r", []int{42, 43})
	if _, _, removed := fake.snapshot(); len(removed) != 0 {
		t.Fatalf("disabled clear removed labels: %v", removed)
	}
	enableInProgress(c, "hive-a", time.Hour)
	c.clearInProgressForPR(context.Background(), "o/r", []int{42, 0, 43})
	added, _, removed := fake.snapshot()
	// Each issue is re-read and only the claim-side labels it carries come
	// off (#42 has none, #43 has the stage and the retired mark), journaled
	// against that read; both move to stage/040-covered-by-pr.
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want the two labels #43 carried", removed)
	}
	for _, p := range removed {
		if !strings.Contains(p, "/issues/43/labels/") || (!strings.Contains(p, "stage/030") && !strings.Contains(p, "in-progress")) {
			t.Errorf("unexpected removal path %q", p)
		}
	}
	if len(added) != 2 || added[0][0] != CoveredByPRLabel || added[1][0] != CoveredByPRLabel {
		t.Fatalf("added = %v, want stage/040 on both issues", added)
	}
}

// sweepServer serves comments for widgets#7 (the claimServer fixture) and
// records label removals.
func sweepServer(t *testing.T, bodies []string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	removed := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		type wireComment struct {
			ID   int               `json:"id"`
			Body string            `json:"body"`
			User map[string]string `json:"user"`
		}
		out := make([]wireComment, 0, len(bodies))
		for i, b := range bodies {
			out = append(out, wireComment{ID: i + 1, Body: b, User: map[string]string{"login": claimBotLogin}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			removed = append(removed, r.URL.Path)
			mu.Unlock()
			_, _ = io.WriteString(w, `[]`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &removed
}

func inProgressIssue() Issue {
	i := claimIssue()
	i.Labels = []string{"bug", InProgressLabel}
	return i
}

func TestSweepInProgressIssues_LiveMarkWithholds(t *testing.T) {
	server, removed := sweepServer(t, []string{InProgressCommentBody("scanner", "hive-b", inProgressNow.Add(-10*time.Minute), inProgressNow.Add(110*time.Minute))})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableInProgress(c, "hive-a", 2*time.Hour)
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{inProgressIssue(), {Repo: "widgets", Number: 8, Labels: []string{"bug"}}})}

	withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if withheld != 1 {
		t.Fatalf("withheld = %d, want 1", withheld)
	}
	if len(result.Issues.Items) != 1 || result.Issues.Items[0].Number != 8 || result.Issues.Count != 1 {
		t.Fatalf("remaining issues = %+v", result.Issues)
	}
	if len(*removed) != 0 {
		t.Fatalf("live mark must keep its label, removed %v", *removed)
	}
	details := result.WorkDetailsByRepo["widgets"]
	found := false
	for _, d := range details.Issues {
		if d.Number == 7 && d.Bucket == inProgressWorkBucket && strings.Contains(d.Reason, "scanner") {
			found = true
		}
	}
	if !found {
		t.Fatalf("work details missing the in_progress bucket: %+v", details)
	}
}

func TestSweepInProgressIssues_LapsedMarkClearsLabel(t *testing.T) {
	server, removed := sweepServer(t, []string{InProgressCommentBody("scanner", "hive-b", inProgressNow.Add(-3*time.Hour), inProgressNow.Add(-time.Hour))})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableInProgress(c, "hive-a", 2*time.Hour)
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{inProgressIssue()})}

	if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, nil); withheld != 0 {
		t.Fatalf("withheld = %d, want 0", withheld)
	}
	if len(result.Issues.Items) != 1 || issueHasLabel(result.Issues.Items[0].Labels, InProgressLabel) {
		t.Fatalf("lapsed issue should stay actionable without the label: %+v", result.Issues.Items)
	}
	if len(*removed) != 1 {
		t.Fatalf("expected one label removal, got %v", *removed)
	}
}

func TestSweepInProgressIssues_NoMarkerClearsLabel(t *testing.T) {
	server, removed := sweepServer(t, []string{"just a human comment"})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableInProgress(c, "hive-a", 2*time.Hour)
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{inProgressIssue()})}
	if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, nil); withheld != 0 {
		t.Fatalf("withheld = %d, want 0", withheld)
	}
	if len(*removed) != 1 {
		t.Fatalf("expected one label removal, got %v", *removed)
	}
}

func TestSweepInProgressIssues_PREvidenceSupersedesWithoutFetch(t *testing.T) {
	server, removed := sweepServer(t, []string{InProgressCommentBody("scanner", "hive-b", inProgressNow, inProgressNow.Add(time.Hour))})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableInProgress(c, "hive-a", 2*time.Hour)
	issue := inProgressIssue()
	issue.Labels = append(issue.Labels, CoveredByPRLabel)
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{issue})}
	if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, nil); withheld != 0 {
		t.Fatalf("withheld = %d, want 0: a PR supersedes the start mark", withheld)
	}
	if len(*removed) != 1 {
		t.Fatalf("expected one label removal, got %v", *removed)
	}
	if got := result.Issues.Items[0].Labels; issueHasLabel(got, InProgressLabel) || !issueHasLabel(got, CoveredByPRLabel) {
		t.Fatalf("labels after sweep = %v", got)
	}
}

func TestSweepInProgressIssues_DisabledOrUnlabelledIsNoop(t *testing.T) {
	server, removed := sweepServer(t, nil)
	c := newTestClient(t, server, "acme", []string{"widgets"})
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{inProgressIssue()})}
	if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, nil); withheld != 0 {
		t.Fatalf("disabled sweep withheld %d", withheld)
	}
	enableInProgress(c, "hive-a", time.Hour)
	plain := &ActionableResult{Issues: IssueResultFromItems([]Issue{{Repo: "widgets", Number: 7, Labels: []string{"bug"}}})}
	if withheld := SweepInProgressIssues(context.Background(), c, plain, inProgressNow, nil); withheld != 0 {
		t.Fatalf("unlabelled sweep withheld %d", withheld)
	}
	if len(*removed) != 0 {
		t.Fatalf("no-op sweeps removed labels: %v", *removed)
	}
	if SweepInProgressIssues(context.Background(), nil, plain, inProgressNow, nil) != 0 {
		t.Fatal("nil client must be a no-op")
	}
}

func TestSweepInProgressIssues_FetchFailureWithholdsOneCycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableInProgress(c, "hive-a", time.Hour)
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{inProgressIssue()})}
	if withheld := SweepInProgressIssues(context.Background(), c, result, inProgressNow, slog.New(slog.NewTextHandler(io.Discard, nil))); withheld != 1 {
		t.Fatalf("withheld = %d, want 1 (fail closed)", withheld)
	}
}

func TestInProgressSetting_DefaultTTL(t *testing.T) {
	c := &Client{}
	if set := c.inProgressSetting(); set.Enabled {
		t.Fatal("unset resolver must disable")
	}
	c.SetInProgress(func() InProgressSetting { return InProgressSetting{Enabled: true} })
	if set := c.inProgressSetting(); set.TTL != claims.DefaultAgentTTL {
		t.Fatalf("TTL = %v, want agent default", set.TTL)
	}
	var nilClient *Client
	nilClient.SetInProgress(nil) // must not panic
	if nilClient.inProgressSetting().Enabled {
		t.Fatal("nil client must disable")
	}
}
