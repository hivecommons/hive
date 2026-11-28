package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/reviewgate"
)

// The GitHub fakes of this package's older tests predate the open
// change-request gate (hivecommons/hive#20067) and do not serve the review
// listing, so the package's tests run with a gate that sees no change
// requests; every test below restores the real check.
func init() {
	openChangeRequestsCheck = func(context.Context, *gh.Client, string, string, int) ([]reviewgate.Block, error) { return nil, nil }
}

// realChangesRequestedGate restores the real check for one test.
func realChangesRequestedGate(t *testing.T) {
	t.Helper()
	prev := openChangeRequestsCheck
	openChangeRequestsCheck = reviewgate.CheckGitHub
	t.Cleanup(func() { openChangeRequestsCheck = prev })
}

// crFake is a GitHub fake for the open change-request gate
// (hivecommons/hive#20067): PR o/r#7 with configurable reviews and
// collaborator permissions, counting every write the gate must prevent.
type crFake struct {
	mu        sync.Mutex
	reviews   []map[string]any
	perms     map[string]string
	labelSet  []string
	merges    int
	approvals int
	labels    int
	comments  int
}

func (f *crFake) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.merges + f.approvals + f.labels + f.comments
}

func (f *crFake) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode(f.reviews)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/collaborators/"):
			login := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/o/r/collaborators/"), "/permission")
			p, ok := f.perms[login]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": p})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "state": "open", "title": "t", "body": "b",
				"head": map[string]string{"sha": "head7"}, "base": map[string]string{"ref": "main"},
				"user": map[string]string{"login": "contributor"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			labels := []map[string]string{}
			for _, l := range f.labelSet {
				labels = append(labels, map[string]string{"name": l})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "labels": labels})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/o/r/pulls/7/merge":
			f.merges++
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "merged7"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			f.approvals++
			_, _ = io.WriteString(w, `{"id":100,"state":"APPROVED"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/labels":
			f.labels++
			_, _ = io.WriteString(w, `[{"name":"lgtm"}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			f.comments++
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func crReview(id int64, login, state string, minute int) map[string]any {
	return map[string]any{
		"id": id, "user": map[string]string{"login": login}, "state": state, "commit_id": "head7",
		"submitted_at": fmt.Sprintf("2026-11-26T10:%02d:00Z", minute),
	}
}

// count reads one counter under the fake's lock.
func (f *crFake) count(n *int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *n
}

type auditSink struct {
	mu      sync.Mutex
	actions []string
	details []string
}

func (a *auditSink) record(action, detail, _ string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, action)
	a.details = append(a.details, detail)
}

func (a *auditSink) find(action, needle string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, got := range a.actions {
		if got == action && strings.Contains(a.details[i], needle) {
			return true
		}
	}
	return false
}

func crClient(t *testing.T, f *crFake) (*Client, *auditSink) {
	t.Helper()
	realChangesRequestedGate(t)
	srv := f.serve(t)
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sink := &auditSink{}
	c.SetAttributionAudit(sink.record)
	return c, sink
}

func openChangeRequest() *crFake {
	return &crFake{
		reviews: []map[string]any{crReview(42, "maint", "CHANGES_REQUESTED", 5)},
		perms:   map[string]string{"maint": "write"},
	}
}

// Every GitHub write path refuses while a writer's change request is open,
// and audits the attempt with the reviewer and the review ID.
func TestChangesRequestedBlocksEveryGitHubPath(t *testing.T) {
	paths := map[string]func(c *Client) error{
		"merge relay": func(c *Client) error {
			_, err := c.MergePR(context.Background(), "o/r", 7, "squash", "head7")
			return err
		},
		"dashboard merge": func(c *Client) error {
			_, err := c.MergePRVia(context.Background(), "o/r", 7, "", "head7", PRAuditPathDashboard)
			return err
		},
		"auto-merge queue approval": func(c *Client) error {
			return c.QueuePRAutoMerge(context.Background(), "o/r", 7, "alice")
		},
		"lgtm label": func(c *Client) error {
			return c.AddLabels(context.Background(), "o/r", 7, []string{"lgtm"})
		},
		"approved label": func(c *Client) error {
			return c.AddLabels(context.Background(), "o/r", 7, []string{"Approved"})
		},
		"forge label relay": func(c *Client) error {
			return c.GateApprovalLabels(context.Background(), "o/r", 7, []string{"lgtm"})
		},
		"dashboard approve": func(c *Client) error {
			_, err := c.RecordOperatorPullRequestApproval(context.Background(), "o/r", 7, "owner")
			return err
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			f := openChangeRequest()
			c, sink := crClient(t, f)
			err := run(c)
			var blocked *reviewgate.BlockedError
			if !errors.As(err, &blocked) || !errors.Is(err, reviewgate.ErrChangesRequested) {
				t.Fatalf("err = %v, want a *reviewgate.BlockedError", err)
			}
			if len(blocked.Blocks) != 1 || blocked.Blocks[0].Reviewer != "maint" || blocked.Blocks[0].ReviewID != 42 {
				t.Fatalf("blocks = %+v, want maint's review 42", blocked.Blocks)
			}
			if n := f.writes(); n != 0 {
				t.Fatalf("%d writes reached GitHub, want none", n)
			}
			if !sink.find(AuditActionChangesRequestedGate, "outcome=blocked") || !sink.find(AuditActionChangesRequestedGate, "review_ids=42") || !sink.find(AuditActionChangesRequestedGate, "reviewers=maint") {
				t.Fatalf("audit = %v %v, want a blocked gate record naming maint and review 42", sink.actions, sink.details)
			}
		})
	}
}

// The review swarm's approval becomes a suppressed verdict: no APPROVE
// review reaches GitHub, and the gate is audited.
func TestChangesRequestedSuppressesReviewSwarmApproval(t *testing.T) {
	realChangesRequestedGate(t)
	f := openChangeRequest()
	srv := f.serve(t)
	c := reviewTestClient(t, srv.URL)
	sink := &auditSink{}
	c.SetAttributionAudit(sink.record)
	dir := withReviewDir(t)
	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 7, Event: "approve", Agent: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if f.count(&f.approvals) != 0 {
		t.Fatalf("%d APPROVE reviews submitted, want none", f.approvals)
	}
	if !sink.find(AuditActionChangesRequestedGate, "outcome=blocked") {
		t.Fatalf("audit = %v, want a blocked gate record", sink.actions)
	}
}

func TestChangesRequestedReleased(t *testing.T) {
	for name, f := range map[string]*crFake{
		"approval from the same reviewer": {
			reviews: []map[string]any{crReview(42, "maint", "CHANGES_REQUESTED", 5), crReview(43, "maint", "APPROVED", 9)},
			perms:   map[string]string{"maint": "write"},
		},
		"dismissed review": {
			reviews: []map[string]any{crReview(42, "maint", "DISMISSED", 5)},
			perms:   map[string]string{"maint": "write"},
		},
		"change request from a non-writer": {
			reviews: []map[string]any{crReview(42, "passerby", "CHANGES_REQUESTED", 5)},
			perms:   map[string]string{"passerby": "read"},
		},
		"change request from a non-collaborator": {
			reviews: []map[string]any{crReview(42, "stranger", "CHANGES_REQUESTED", 5)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, sink := crClient(t, f)
			res, err := c.MergePRVia(context.Background(), "o/r", 7, "merge", "head7", PRAuditPathDashboard)
			if err != nil {
				t.Fatalf("MergePRVia: %v", err)
			}
			if !res.Merged || f.count(&f.merges) != 1 {
				t.Fatalf("merged=%v merges=%d, want one merge", res.Merged, f.count(&f.merges))
			}
			if sink.find(AuditActionChangesRequestedGate, "") {
				t.Fatalf("audit = %v, want no gate record when nothing blocks", sink.actions)
			}
		})
	}
}

func TestChangesRequestedNonApprovalLabelPasses(t *testing.T) {
	f := openChangeRequest()
	c, _ := crClient(t, f)
	if err := c.AddLabels(context.Background(), "o/r", 7, []string{"kind/bug"}); err != nil {
		t.Fatalf("AddLabels(kind/bug): %v", err)
	}
	if f.count(&f.labels) != 1 {
		t.Fatalf("labels = %d, want the non-approval label written", f.labels)
	}
}

func TestChangesRequestedOperatorOverride(t *testing.T) {
	f := openChangeRequest()
	c, sink := crClient(t, f)
	t.Cleanup(func() { reviewgate.ClearOverride("o/r", 7) })
	blocks, err := c.OpenChangeRequests(context.Background(), "o/r", 7)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("OpenChangeRequests = %v, %v; want maint's change request", blocks, err)
	}
	if _, err := reviewgate.RecordOverride("o/r", 7, "owner", "security fix verified out of band", blocks); err != nil {
		t.Fatal(err)
	}
	res, err := c.MergePRVia(context.Background(), "o/r", 7, "merge", "head7", PRAuditPathDashboard)
	if err != nil || !res.Merged {
		t.Fatalf("MergePRVia with override = %+v, %v; want merged", res, err)
	}
	if !sink.find(AuditActionChangesRequestedGate, "outcome=overridden") || !sink.find(AuditActionChangesRequestedGate, "reason=security fix verified out of band") {
		t.Fatalf("audit = %v %v, want an overridden record with the reason", sink.actions, sink.details)
	}
}

func TestChangesRequestedUnreadableFailsClosed(t *testing.T) {
	realChangesRequestedGate(t)
	var mu sync.Mutex
	merges := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/pulls/7/reviews":
			http.Error(w, "down", http.StatusInternalServerError)
		case r.Method == http.MethodPut:
			mu.Lock()
			merges++
			mu.Unlock()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sink := &auditSink{}
	c.SetAttributionAudit(sink.record)
	_, err := c.MergePRVia(context.Background(), "o/r", 7, "squash", "head7", PRAuditPathRelay)
	if !errors.Is(err, reviewgate.ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if merges != 0 {
		t.Fatalf("merges = %d, want none when reviews cannot be read", merges)
	}
	if !sink.find(AuditActionChangesRequestedGate, "outcome=unreadable") {
		t.Fatalf("audit = %v, want an unreadable record", sink.actions)
	}
}

// A sentinel alert blocks the hive's own `lgtm` / `approved` writes the same
// way it blocks the queue approval: writing either hands the PR to Tide,
// which does not know about the sentinel (hivecommons/hive#20064 merged so).
func TestSentinelAlertBlocksApprovalLabelWrites(t *testing.T) {
	f := &crFake{labelSet: []string{"Sentinel-Alert"}}
	c, sink := crClient(t, f)
	c.SetSentinelAlertLabel("sentinel-alert")
	for _, label := range []string{"lgtm", "approved"} {
		if err := c.AddLabels(context.Background(), "o/r", 7, []string{label}); err == nil || !strings.Contains(err.Error(), "sentinel-alert") {
			t.Fatalf("AddLabels(%s) err = %v, want the sentinel refusal", label, err)
		}
		if err := c.GateApprovalLabels(context.Background(), "o/r", 7, []string{label}); err == nil {
			t.Fatalf("GateApprovalLabels(%s) = nil, want the sentinel refusal", label)
		}
	}
	if f.count(&f.labels) != 0 {
		t.Fatal("an approval label reached GitHub on a sentinel-labelled PR")
	}
	if !sink.find(AuditActionSentinelBlockedApproval, "label=sentinel-alert") {
		t.Fatalf("audit = %v, want sentinel-blocked-approval", sink.actions)
	}
	// Without the sentinel label the same write goes through.
	f.mu.Lock()
	f.labelSet = nil
	f.mu.Unlock()
	if err := c.AddLabels(context.Background(), "o/r", 7, []string{"lgtm"}); err != nil {
		t.Fatalf("AddLabels(lgtm) without sentinel: %v", err)
	}
}

// The swarm's approval supersedes the hive's own change request: a
// REQUEST_CHANGES the App posted earlier does not deadlock its approval.
func TestReviewSwarmApprovalSupersedesOwnChangeRequest(t *testing.T) {
	realChangesRequestedGate(t)
	f := &crFake{
		reviews: []map[string]any{crReview(42, "hive-app[bot]", "CHANGES_REQUESTED", 5)},
		perms:   map[string]string{"hive-app[bot]": "write"},
	}
	srv := f.serve(t)
	c := reviewTestClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	dir := withReviewDir(t)
	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 7, Event: "approve", Agent: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if f.count(&f.approvals) != 1 {
		t.Fatalf("approvals = %d, want the App's approval posted over its own change request", f.count(&f.approvals))
	}
}

// A failed review read leaves the swarm's approval request for a retry
// instead of downgrading the verdict for good.
func TestReviewSwarmApprovalRetriesUnreadableReviews(t *testing.T) {
	realChangesRequestedGate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/pulls/7/reviews" && r.Method == http.MethodGet:
			http.Error(w, "down", http.StatusInternalServerError)
		case r.Method == http.MethodPost:
			t.Errorf("POST %s with unreadable reviews", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)
	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 7, Event: "approve", Agent: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("request file gone after an unreadable gate (%v); want it kept for a retry", err)
	}
}

// The agents' label relay never honours an operator override.
func TestLabelRelayIgnoresOperatorOverride(t *testing.T) {
	f := openChangeRequest()
	c, _ := crClient(t, f)
	t.Cleanup(func() { reviewgate.ClearOverride("o/r", 7) })
	if _, err := reviewgate.RecordOverride("o/r", 7, "owner", "why", []reviewgate.Block{{Reviewer: "maint", ReviewID: 42}}); err != nil {
		t.Fatal(err)
	}
	if err := c.GateApprovalLabels(context.Background(), "o/r", 7, []string{"lgtm"}); !errors.Is(err, reviewgate.ErrChangesRequested) {
		t.Fatalf("relay gate = %v, want the change request to block despite the override", err)
	}
}
