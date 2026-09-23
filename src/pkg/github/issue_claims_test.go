package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
)

// Tests for hivecommons/hive#8380: issue claims are read at enumeration time
// from the issue's own comments (or assignee), cached on updated_at, and are
// invisible — no fetch, no fields — while governor.claims.enabled is off.

var (
	claimNow     = time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	claimUpdated = claimNow.Add(-30 * time.Minute)
)

// claimServer serves one issue's comments and counts how often they are
// fetched, so a test can prove the cache (or the off switch) held.
func claimServer(t *testing.T, bodies []string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		type wireComment struct {
			ID   int    `json:"id"`
			Body string `json:"body"`
		}
		out := make([]wireComment, 0, len(bodies))
		for i, b := range bodies {
			out = append(out, wireComment{ID: i + 1, Body: b})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &fetches
}

func claimIssue() Issue {
	return Issue{Repo: "widgets", Number: 7, Title: "t", UpdatedAt: claimUpdated}
}

func enableClaims(c *Client, ttl time.Duration) {
	c.SetIssueClaims(func() (bool, time.Duration) { return true, ttl })
}

// Flag off: nothing is fetched and the envelope is byte-identical to one
// that never saw this code.
func TestAnnotateIssueClaims_OffIsByteIdenticalAndFetchesNothing(t *testing.T) {
	server, fetches := claimServer(t, []string{issueclaim.CommentBody("alice", claimUpdated, claimNow.Add(time.Hour))})
	c := newTestClient(t, server, "acme", []string{"widgets"})

	before, _ := json.Marshal(claimIssue())
	issues := []Issue{claimIssue()}
	issues[0].Assignees = []string{"bob"}
	c.annotateIssueClaims(context.Background(), "acme", "widgets", issues, claimNow)
	if fetches.Load() != 0 {
		t.Fatalf("claims off must fetch no comments, fetched %d times", fetches.Load())
	}
	issues[0].Assignees = nil
	after, _ := json.Marshal(issues[0])
	if string(before) != string(after) {
		t.Fatalf("claims off must leave the envelope untouched:\n%s\n%s", before, after)
	}
	for _, key := range []string{"claimed_by", "claim_expires_at", "claim_source"} {
		if strings.Contains(string(before), key) {
			t.Fatalf("claim key %s must not appear with claims off: %s", key, before)
		}
	}
	if n := FilterLiveIssueClaims(&ActionableResult{Issues: IssueResultFromItems(issues)}, claimNow, nil); n != 0 {
		t.Fatalf("filter with claims off withheld %d", n)
	}
}

// A live marker comment claims the issue; the fields carry who and until when.
func TestAnnotateIssueClaims_LiveMarkerClaims(t *testing.T) {
	expires := claimNow.Add(time.Hour)
	server, fetches := claimServer(t, []string{"unrelated", issueclaim.CommentBody("alice", claimUpdated, expires)})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableClaims(c, issueclaim.DefaultTTL)

	issues := []Issue{claimIssue()}
	c.annotateIssueClaims(context.Background(), "acme", "widgets", issues, claimNow)
	if fetches.Load() != 1 {
		t.Fatalf("expected one comment fetch, got %d", fetches.Load())
	}
	got := issues[0]
	if got.ClaimedBy != "alice" || got.ClaimSource != issueclaim.SourceMarker {
		t.Fatalf("claim not read from marker: %+v", got)
	}
	if got.ClaimExpiresAt == nil || !got.ClaimExpiresAt.Equal(expires) {
		t.Fatalf("claim_expires_at = %v, want %v", got.ClaimExpiresAt, expires)
	}
	raw, _ := json.Marshal(got)
	for _, key := range []string{`"claimed_by":"alice"`, `"claim_expires_at":"2026-09-23T03:00:00Z"`, `"claim_source":"marker"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("envelope missing %s: %s", key, raw)
		}
	}
	if claim, live := got.LiveClaim(claimNow); !live || claim.Identity != "alice" {
		t.Fatalf("LiveClaim = %+v,%v", claim, live)
	}
	if _, live := got.LiveClaim(expires); live {
		t.Fatal("LiveClaim must be false at the expiry instant")
	}
}

// An expired marker releases the issue — and does NOT fall back to the
// assignee, because the explicit claim explicitly lapsed.
func TestAnnotateIssueClaims_ExpiredMarkerReleases(t *testing.T) {
	server, _ := claimServer(t, []string{issueclaim.CommentBody("alice", claimNow.Add(-2*time.Hour), claimNow.Add(-time.Minute))})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableClaims(c, issueclaim.DefaultTTL)

	issues := []Issue{claimIssue()}
	issues[0].Assignees = []string{"bob"}
	c.annotateIssueClaims(context.Background(), "acme", "widgets", issues, claimNow)
	if issues[0].ClaimedBy != "" || issues[0].ClaimExpiresAt != nil {
		t.Fatalf("expired marker must leave the issue unclaimed, got %+v", issues[0])
	}
}

// With no marker, the assignee holds the issue for ttl from its last activity.
func TestAnnotateIssueClaims_AssigneeFallback(t *testing.T) {
	server, _ := claimServer(t, []string{"just talk"})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableClaims(c, time.Hour)

	issues := []Issue{claimIssue()}
	issues[0].Assignees = []string{"bob"}
	c.annotateIssueClaims(context.Background(), "acme", "widgets", issues, claimNow)
	if issues[0].ClaimedBy != "bob" || issues[0].ClaimSource != issueclaim.SourceAssignee {
		t.Fatalf("assignee must claim when no marker exists, got %+v", issues[0])
	}
	if want := claimUpdated.Add(time.Hour); issues[0].ClaimExpiresAt == nil || !issues[0].ClaimExpiresAt.Equal(want) {
		t.Fatalf("assignee claim expiry = %v, want %v", issues[0].ClaimExpiresAt, want)
	}

	// Past the ttl the same assignee no longer holds it.
	stale := []Issue{claimIssue()}
	stale[0].Assignees = []string{"bob"}
	c.annotateIssueClaims(context.Background(), "acme", "widgets", stale, claimUpdated.Add(2*time.Hour))
	if stale[0].ClaimedBy != "" {
		t.Fatalf("assignee claim past ttl must release, got %+v", stale[0])
	}
}

// The comment fetch happens once per updated_at, not once per enumeration;
// activity on the issue invalidates it.
func TestAnnotateIssueClaims_CacheKeyedOnUpdatedAt(t *testing.T) {
	server, fetches := claimServer(t, []string{issueclaim.CommentBody("alice", claimUpdated, claimNow.Add(time.Hour))})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableClaims(c, issueclaim.DefaultTTL)

	for i := 0; i < 3; i++ {
		issues := []Issue{claimIssue()}
		c.annotateIssueClaims(context.Background(), "acme", "widgets", issues, claimNow)
		if issues[0].ClaimedBy != "alice" {
			t.Fatalf("pass %d: claim lost, got %+v", i, issues[0])
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("same updated_at must hit the cache, fetched %d times", fetches.Load())
	}
	moved := []Issue{claimIssue()}
	moved[0].UpdatedAt = claimUpdated.Add(time.Minute)
	c.annotateIssueClaims(context.Background(), "acme", "widgets", moved, claimNow)
	if fetches.Load() != 2 {
		t.Fatalf("new activity must refetch, fetched %d times", fetches.Load())
	}
	// A cached claim that has since expired releases without a fetch.
	later := []Issue{claimIssue()}
	later[0].UpdatedAt = moved[0].UpdatedAt
	c.annotateIssueClaims(context.Background(), "acme", "widgets", later, claimNow.Add(2*time.Hour))
	if later[0].ClaimedBy != "" || fetches.Load() != 2 {
		t.Fatalf("expired cached claim must release without a fetch: %+v fetches=%d", later[0], fetches.Load())
	}
}

// A fetch failure fails OPEN: the issue is left unclaimed and the sweep
// continues.
func TestAnnotateIssueClaims_FetchFailureLeavesUnclaimed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, "acme", []string{"widgets"})
	c.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	enableClaims(c, issueclaim.DefaultTTL)

	issues := []Issue{claimIssue()}
	c.annotateIssueClaims(context.Background(), "acme", "widgets", issues, claimNow)
	if issues[0].ClaimedBy != "" {
		t.Fatalf("fetch failure must not claim, got %+v", issues[0])
	}
}

func TestFilterLiveIssueClaims(t *testing.T) {
	live := claimNow.Add(time.Hour)
	lapsed := claimNow.Add(-time.Minute)
	items := []Issue{
		{Repo: "widgets", Number: 1, ClaimedBy: "alice", ClaimExpiresAt: &live, ClaimSource: issueclaim.SourceMarker},
		{Repo: "widgets", Number: 2, ClaimedBy: "bob", ClaimExpiresAt: &lapsed},
		{Repo: "widgets", Number: 3},
	}
	result := &ActionableResult{Issues: IssueResultFromItems(items)}
	if n := FilterLiveIssueClaims(result, claimNow, slog.New(slog.NewTextHandler(io.Discard, nil))); n != 1 {
		t.Fatalf("withheld %d, want 1", n)
	}
	if len(result.Issues.Items) != 2 || result.Issues.Count != 2 {
		t.Fatalf("kept %d/%d, want 2 (expired claim released, unclaimed kept)", len(result.Issues.Items), result.Issues.Count)
	}
	for _, issue := range result.Issues.Items {
		if issue.Number == 1 {
			t.Fatal("live-claimed issue #1 was not withheld")
		}
	}
	if n := FilterLiveIssueClaims(nil, claimNow, nil); n != 0 {
		t.Fatalf("nil result withheld %d", n)
	}
}

// The SetIssueClaims setting is read live and a zero ttl takes the default.
func TestIssueClaimsSetting(t *testing.T) {
	var c *Client
	if on, _ := c.issueClaimsSetting(); on {
		t.Fatal("nil client must report claims off")
	}
	c = &Client{}
	if on, _ := c.issueClaimsSetting(); on {
		t.Fatal("no setting must report claims off")
	}
	c.SetIssueClaims(func() (bool, time.Duration) { return true, 0 })
	on, ttl := c.issueClaimsSetting()
	if !on || ttl != issueclaim.DefaultTTL {
		t.Fatalf("setting = %v,%v; want on with the default ttl", on, ttl)
	}
	// The nil-receiver setter is a no-op rather than a panic.
	var nilClient *Client
	nilClient.SetIssueClaims(nil)
}

// The dashboard seam: tier -> may-comment follows the agentmode ladder
// (ISSUES_ONLY rung) and unknown tiers fail closed.
func TestIssueClaimTierCanComment(t *testing.T) {
	for tier, want := range map[string]bool{
		"advisor": false, "reviewer": false, "newcomer": true, "contributor": true,
		"trusted": true, "merger": true, "revoked": false, "": false,
	} {
		if got, _ := IssueClaimTierCanComment(tier); got != want {
			t.Errorf("IssueClaimTierCanComment(%q) = %v, want %v", tier, got, want)
		}
	}
	if _, mode := IssueClaimTierCanComment("contributor"); mode != "ISSUES_AND_PRS" {
		t.Errorf("mode name = %q, want ISSUES_AND_PRS", mode)
	}
	body := IssueClaimCommentBody("x", claimNow, claimNow.Add(IssueClaimDefaultTTL))
	if claim, ok := issueclaim.ParseMarker(body); !ok || claim.Identity != "x" || claim.Source != IssueClaimSourceMarker {
		t.Errorf("seam comment body must round-trip: %+v %v", claim, ok)
	}
}
