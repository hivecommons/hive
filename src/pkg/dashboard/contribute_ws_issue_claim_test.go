package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
)

// Tests for hivecommons/hive#8380: an issue someone has CLAIMED on the issue
// itself — a `hive-claim` marker comment or an assignee — is withheld from the
// contribute queue while the claim is live, released when it expires, and
// invisible while governor.claims.enabled is off. A relay contributor that
// takes a lease asserts its own claim: on the issue when its tier may write
// comments, on the lease alone below that.

const (
	issueClaimRepoFull = "projectbluefin/dakota"
	issueClaimHolder   = "someone-else"
	claimedNumber      = 353
	unclaimedNumber    = 360
)

// issueClaimStatus seeds two admissible issues; #353 carries the enumerator's
// claim fields when claimedBy is set, #360 never does.
func issueClaimStatus(s *Server, claimedBy string, expires time.Time) {
	claimed := map[string]any{
		"number": float64(claimedNumber),
		"title":  "the issue someone is already on",
		"url":    "https://github.com/projectbluefin/dakota/issues/353",
		"author": "someone",
	}
	if claimedBy != "" {
		claimed["claimed_by"] = claimedBy
		claimed["claim_expires_at"] = expires.UTC().Format(time.RFC3339)
		claimed["claim_source"] = issueclaim.SourceMarker
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "dakota",
			Full: issueClaimRepoFull,
			ActionableIssues: []any{
				claimed,
				map[string]any{
					"number": float64(unclaimedNumber),
					"title":  "an unclaimed issue",
					"url":    "https://github.com/projectbluefin/dakota/issues/360",
					"author": "someone",
				},
			},
		}},
	}
	s.statusMu.Unlock()
}

func enableIssueClaims(s *Server) {
	s.deps.Config.Governor.Claims.Enabled = true
}

func issueClaimConn(tier string) *ContributorConnection {
	return &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "relay-bot", ContributorID: "c-relay", TrustTier: tier},
		lastPong: time.Now(),
	}
}

// claimRecorder is the forge seam stand-in: it records every claim comment
// the hub posts, or fails them on demand.
type claimRecorder struct {
	mu    sync.Mutex
	posts []claimPost
	fail  error
}

type claimPost struct {
	repo   string
	number int
	body   string
}

func (r *claimRecorder) CreateIssueComment(_ context.Context, repo string, number int, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.posts = append(r.posts, claimPost{repo: repo, number: number, body: body})
	return nil
}

func (r *claimRecorder) all() []claimPost {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]claimPost(nil), r.posts...)
}

// A live claim withholds the issue from assignment and from the ready queue,
// and the withheld listing names the claimant and the expiry.
func TestIssueClaim_LiveClaimWithheld(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	hub.claimCommenter = &claimRecorder{}
	expires := time.Now().Add(time.Hour)
	issueClaimStatus(s, issueClaimHolder, expires)

	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 1 || queue[0].Number != unclaimedNumber {
		t.Fatalf("ReadyQueue must withhold the claimed issue, got %+v", queue)
	}

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	item, ok := findWithheld(snap.withheld, issueClaimRepoFull+"#353")
	if !ok {
		t.Fatalf("claimed issue is withheld but unexplained; collected %+v", snap.withheld)
	}
	if item.Reason != contributorAdmissionReasonIssueClaim {
		t.Fatalf("reason = %q, want %q", item.Reason, contributorAdmissionReasonIssueClaim)
	}
	if item.ClaimedBy != issueClaimHolder {
		t.Errorf("claimed_by = %q, want %q", item.ClaimedBy, issueClaimHolder)
	}
	if want := expires.UTC().Format(time.RFC3339); item.ClaimExpiresAt != want {
		t.Errorf("claim_expires_at = %q, want %q", item.ClaimExpiresAt, want)
	}
	if item.Detail == "" {
		t.Error("withheld row needs operator-facing prose")
	}
	raw, _ := json.Marshal(item)
	for _, key := range []string{`"claimed_by":"someone-else"`, `"claim_expires_at":"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("withheld payload missing %s: %s", key, raw)
		}
	}

	msg := hub.selectTask(issueClaimConn("contributor"))
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", msg)
	}
	if msg.Number != unclaimedNumber {
		t.Fatalf("claimed issue was assigned anyway: got #%d, want #%d", msg.Number, unclaimedNumber)
	}
}

// An expired claim releases the issue: it is offered first again, and no
// withheld row is recorded for it.
func TestIssueClaim_ExpiredClaimReleased(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	hub.claimCommenter = &claimRecorder{}
	issueClaimStatus(s, issueClaimHolder, time.Now().Add(-time.Minute))

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	if _, ok := findWithheld(snap.withheld, issueClaimRepoFull+"#353"); ok {
		t.Fatal("an expired claim must not withhold the issue")
	}
	if len(snap.queue) != 2 {
		t.Fatalf("both issues must be ready once the claim lapsed, got %+v", snap.queue)
	}
	msg := hub.selectTask(issueClaimConn("contributor"))
	if msg == nil || msg.Type != "task_assign" || msg.Number != claimedNumber {
		t.Fatalf("expired claim must release #353 for assignment, got %+v", msg)
	}
}

// Every issue claimed yields the same explicit no_matching_work negative-ack
// an all-PR-claimed queue yields — never a duplicate assignment.
func TestIssueClaim_AllClaimedYieldsNoMatchingWork(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	hub.claimCommenter = &claimRecorder{}
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{Name: "dakota", Full: issueClaimRepoFull, ActionableIssues: []any{
		map[string]any{"number": float64(1), "title": "a", "url": "u", "author": "x", "claimed_by": "p", "claim_expires_at": expires},
		map[string]any{"number": float64(2), "title": "b", "url": "u", "author": "x", "claimed_by": "q", "claim_expires_at": expires},
	}}}}
	s.statusMu.Unlock()

	msg := hub.selectTask(issueClaimConn("contributor"))
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("expected task_unavailable/no_matching_work, got %+v", msg)
	}
}

// Flag off: the claim fields on the envelope are ignored entirely. The queue,
// the withheld listing and the assignment are byte-for-byte what they are for
// an envelope that never carried the fields, and no claim comment is posted.
func TestIssueClaim_FlagOffIsByteIdentical(t *testing.T) {
	snapshotJSON := func(t *testing.T, claimedBy string) (queue, withheld string, assigned int, posts []claimPost) {
		t.Helper()
		hub, s := covK2Hub(t)
		recorder := &claimRecorder{}
		hub.claimCommenter = recorder
		if s.deps.Config.Governor.Claims.Enabled {
			t.Fatal("claims must be off by default")
		}
		issueClaimStatus(s, claimedBy, time.Now().Add(time.Hour))
		snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
		q, _ := json.Marshal(snap.queue)
		w, _ := json.Marshal(snap.withheld)
		msg := hub.selectTask(issueClaimConn("trusted"))
		if msg == nil || msg.Type != "task_assign" {
			t.Fatalf("expected task_assign, got %+v", msg)
		}
		return string(q), string(w), msg.Number, recorder.all()
	}
	withQ, withW, withN, withPosts := snapshotJSON(t, issueClaimHolder)
	plainQ, plainW, plainN, plainPosts := snapshotJSON(t, "")
	if withQ != plainQ {
		t.Errorf("ready queue differs with claims off:\n%s\n%s", withQ, plainQ)
	}
	if withW != plainW {
		t.Errorf("withheld listing differs with claims off:\n%s\n%s", withW, plainW)
	}
	if withN != plainN || withN != claimedNumber {
		t.Errorf("assignment differs with claims off: %d vs %d (want first eligible #%d)", withN, plainN, claimedNumber)
	}
	if len(withPosts) != 0 || len(plainPosts) != 0 {
		t.Errorf("claims off must post nothing, got %d/%d posts", len(withPosts), len(plainPosts))
	}
	for _, raw := range []string{withQ, withW} {
		if strings.Contains(raw, "claimed_by") || strings.Contains(raw, "claim_expires_at") {
			t.Errorf("claim keys leaked into a claims-off payload: %s", raw)
		}
	}
}

// The claim comment is posted only at a tier whose mode may write issue
// comments (agentmode.CanComment); below it the claim is lease-only. Same
// ladder shape as the agentmode tier tests.
func TestIssueClaim_AgentClaimPostedOnlyAtCommentCapableTier(t *testing.T) {
	for _, tc := range []struct {
		tier   string
		posted bool
	}{
		{"advisor", false},
		{"reviewer", false},
		{"newcomer", true},
		{"contributor", true},
		{"trusted", true},
		{"merger", true},
		{"revoked", false}, // unknown tier fails closed
	} {
		t.Run(tc.tier, func(t *testing.T) {
			hub, s := covK2Hub(t)
			enableIssueClaims(s)
			recorder := &claimRecorder{}
			hub.claimCommenter = recorder
			c := issueClaimConn(tc.tier)
			now := time.Now()
			if err := hub.recordLeaseForKey(identityOf(c), "task-1", issueClaimRepoFull, claimedNumber, "", tc.tier, 1, now); err != nil {
				t.Fatalf("recordLease: %v", err)
			}

			claim, posted := hub.recordAgentClaim(context.Background(), c, "task-1", issueClaimRepoFull, claimedNumber, now)
			if posted != tc.posted {
				t.Fatalf("posted = %v, want %v", posted, tc.posted)
			}
			if claim.Identity != "relay-bot" {
				t.Fatalf("claim identity = %q, want the contributor's login", claim.Identity)
			}
			if want := now.Add(issueclaim.DefaultTTL); !claim.ExpiresAt.Equal(want) {
				t.Fatalf("claim expiry = %v, want now+default ttl %v", claim.ExpiresAt, want)
			}
			posts := recorder.all()
			if tc.posted {
				if len(posts) != 1 || posts[0].repo != issueClaimRepoFull || posts[0].number != claimedNumber {
					t.Fatalf("expected one claim comment on %s#%d, got %+v", issueClaimRepoFull, claimedNumber, posts)
				}
				parsed, ok := issueclaim.ParseMarker(posts[0].body)
				if !ok || parsed.Identity != "relay-bot" || !parsed.ExpiresAt.Equal(claim.ExpiresAt.Truncate(time.Second)) {
					t.Fatalf("posted comment must carry a parseable marker for the same claim: ok=%v %+v body=%q", ok, parsed, posts[0].body)
				}
				if claim.Source != issueclaim.SourceMarker {
					t.Errorf("a posted claim reports source %q, want %q", claim.Source, issueclaim.SourceMarker)
				}
			} else {
				if len(posts) != 0 {
					t.Fatalf("tier %s must not comment, posted %+v", tc.tier, posts)
				}
				if claim.Source != issueclaim.SourceLease {
					t.Errorf("a lease-only claim reports source %q, want %q", claim.Source, issueclaim.SourceLease)
				}
			}

			// Either way the lease carries the claim.
			hub.leaseMu.Lock()
			l := hub.leases[leaseKey(identityOf(c), "task-1")]
			hub.leaseMu.Unlock()
			if l == nil || l.claimedBy != "relay-bot" || !l.claimExpiresAt.Equal(claim.ExpiresAt) || l.claimPosted != tc.posted {
				t.Fatalf("lease claim = %+v, want claimant relay-bot, expiry %v, posted %v", l, claim.ExpiresAt, tc.posted)
			}
		})
	}
}

// A failed comment post never refuses the assignment: the claim stays on the
// lease, unposted.
func TestIssueClaim_CommentFailureLeavesLeaseClaim(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	recorder := &claimRecorder{fail: errors.New("forge down")}
	hub.claimCommenter = recorder
	c := issueClaimConn("contributor")
	now := time.Now()
	if err := hub.recordLeaseForKey(identityOf(c), "task-2", issueClaimRepoFull, claimedNumber, "", "contributor", 2, now); err != nil {
		t.Fatalf("recordLease: %v", err)
	}
	claim, posted := hub.recordAgentClaim(context.Background(), c, "task-2", issueClaimRepoFull, claimedNumber, now)
	if posted || claim.Identity != "relay-bot" || claim.Source != issueclaim.SourceLease {
		t.Fatalf("failed post must leave a lease-only claim, got posted=%v %+v", posted, claim)
	}
	hub.leaseMu.Lock()
	l := hub.leases[leaseKey(identityOf(c), "task-2")]
	hub.leaseMu.Unlock()
	if l == nil || l.claimedBy != "relay-bot" || l.claimPosted {
		t.Fatalf("lease = %+v, want an unposted claim by relay-bot", l)
	}
}

// End to end: a relay contributor that takes a lease through selectTask posts
// the claim on the issue it was assigned, with the ttl the operator set.
func TestIssueClaim_SelectTaskPostsClaimForAssignedIssue(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	s.deps.Config.Governor.Claims.TTLS = 1800
	recorder := &claimRecorder{}
	hub.claimCommenter = recorder
	issueClaimStatus(s, "", time.Time{})

	before := time.Now()
	msg := hub.selectTask(issueClaimConn("contributor"))
	if msg == nil || msg.Type != "task_assign" || msg.Number != claimedNumber {
		t.Fatalf("expected #353 assigned, got %+v", msg)
	}
	posts := recorder.all()
	if len(posts) != 1 || posts[0].repo != issueClaimRepoFull || posts[0].number != claimedNumber {
		t.Fatalf("expected one claim comment on the assigned issue, got %+v", posts)
	}
	parsed, ok := issueclaim.ParseMarker(posts[0].body)
	if !ok {
		t.Fatalf("claim comment carries no marker: %q", posts[0].body)
	}
	if parsed.Identity != "relay-bot" {
		t.Errorf("marker identity = %q, want relay-bot", parsed.Identity)
	}
	ttl := parsed.ExpiresAt.Sub(parsed.StartedAt)
	if ttl != 30*time.Minute {
		t.Errorf("marker ttl = %v, want the configured 30m", ttl)
	}
	if parsed.StartedAt.Before(before.Truncate(time.Second)) {
		t.Errorf("marker started %v before the assignment %v", parsed.StartedAt, before)
	}
}

// The runs API exposes the lease's claim as claimed_by / claim_expires_at,
// and the lease registry round-trips it across a restart.
func TestIssueClaim_RunsAndLeaseRegistryCarryClaim(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	hub.claimCommenter = &claimRecorder{}
	c := issueClaimConn("contributor")
	now := time.Now()
	if err := hub.recordLeaseForKeyStage(identityOf(c), "task-3", issueClaimRepoFull, claimedNumber, "", "contributor", StageImplement, 3, now); err != nil {
		t.Fatalf("recordLease: %v", err)
	}
	claim, posted := hub.recordAgentClaim(context.Background(), c, "task-3", issueClaimRepoFull, claimedNumber, now)
	if !posted {
		t.Fatal("contributor tier must post")
	}

	snapshots, err := (&Server{contributeHub: hub}).activeRunLeaseSnapshots(now)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("activeRunLeaseSnapshots = %+v, %v", snapshots, err)
	}
	run := runFromLease(snapshots[0], runPlanSnapshot{}, runHumanReviewHold{})
	if run.ClaimedBy != "relay-bot" || run.ClaimExpiresAt != claim.ExpiresAt.UTC().Format(time.RFC3339) || !run.ClaimPosted {
		t.Fatalf("run claim fields = %q/%q/%v, want relay-bot/%s/true", run.ClaimedBy, run.ClaimExpiresAt, run.ClaimPosted, claim.ExpiresAt.UTC().Format(time.RFC3339))
	}
	raw, _ := json.Marshal(run)
	if !strings.Contains(string(raw), `"claimed_by":"relay-bot"`) || !strings.Contains(string(raw), `"claim_expires_at":"`) {
		t.Fatalf("runs payload missing claim fields: %s", raw)
	}

	// Restart: a fresh hub over the same registry file restores the claim.
	restarted := NewContributeWSHub(hub.logger, s)
	t.Cleanup(restarted.Close)
	restarted.leaseMu.Lock()
	l := restarted.leases[leaseKey(identityOf(c), "task-3")]
	restarted.leaseMu.Unlock()
	if !hub.persistTaskLedgers {
		t.Skip("lease persistence disabled in this environment")
	}
	if l == nil || l.claimedBy != "relay-bot" || !l.claimExpiresAt.Equal(claim.ExpiresAt) || !l.claimPosted {
		t.Fatalf("restored lease claim = %+v, want relay-bot until %v posted", l, claim.ExpiresAt)
	}

	// A run without a claim carries no claim keys at all.
	plain, _ := json.Marshal(runFromLease(runLeaseSnapshot{key: "k", stage: StageImplement}, runPlanSnapshot{}, runHumanReviewHold{}))
	if strings.Contains(string(plain), "claimed_by") || strings.Contains(string(plain), "claim_expires_at") || strings.Contains(string(plain), "claim_posted") {
		t.Fatalf("claim keys must be omitted when no claim is recorded: %s", plain)
	}
}

// setLeaseClaim on a lease that is not there attaches nothing and does not
// panic; recordAgentClaim outside a GitHub issue records nothing.
func TestIssueClaim_NoLeaseNoClaim(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	hub.setLeaseClaim("nobody", "no-task", issueclaim.Claim{Identity: "x"}, true)
	hub.setLeaseClaim("", "", issueclaim.Claim{}, false)
	c := issueClaimConn("contributor")
	if claim, posted := hub.recordAgentClaim(context.Background(), c, "t", "", 0, time.Now()); posted || claim.Identity != "" {
		t.Fatalf("no repo/number must record nothing, got %+v %v", claim, posted)
	}
	if claim, posted := hub.recordAgentClaim(context.Background(), nil, "t", issueClaimRepoFull, 1, time.Now()); posted || claim.Identity != "" {
		t.Fatalf("nil connection must record nothing, got %+v %v", claim, posted)
	}
	var nilHub *ContributeWSHub
	if nilHub.claimsEnabled() || nilHub.claimTTL() != issueclaim.DefaultTTL || nilHub.claimCommenterFor() != nil {
		t.Fatal("nil hub must report claims off with the default ttl and no commenter")
	}
}

// claimFromIssueMap is the single reader: malformed or stale fields never
// produce a claim, and the feature flag gates it before anything else.
func TestIssueClaim_ClaimFromIssueMap(t *testing.T) {
	hub, s := covK2Hub(t)
	now := time.Now()
	live := now.Add(time.Hour).UTC().Format(time.RFC3339)
	if _, ok := hub.claimFromIssueMap(map[string]any{"claimed_by": "a", "claim_expires_at": live}, now); ok {
		t.Fatal("claims off must read no claim")
	}
	enableIssueClaims(s)
	for name, issue := range map[string]map[string]any{
		"no fields":       {},
		"no expiry":       {"claimed_by": "a"},
		"no identity":     {"claim_expires_at": live},
		"bad expiry":      {"claimed_by": "a", "claim_expires_at": "soon"},
		"expired":         {"claimed_by": "a", "claim_expires_at": now.Add(-time.Second).UTC().Format(time.RFC3339)},
		"wrong type":      {"claimed_by": 7, "claim_expires_at": live},
		"wrong type expo": {"claimed_by": "a", "claim_expires_at": 7},
	} {
		if claim, ok := hub.claimFromIssueMap(issue, now); ok {
			t.Errorf("%s: read a claim %+v", name, claim)
		}
	}
	claim, ok := hub.claimFromIssueMap(map[string]any{"claimed_by": "a", "claim_expires_at": live, "claim_source": issueclaim.SourceAssignee}, now)
	if !ok || claim.Identity != "a" || claim.Source != issueclaim.SourceAssignee {
		t.Fatalf("live claim not read: %+v %v", claim, ok)
	}
	if got := hub.claimTTL(); got != issueclaim.DefaultTTL {
		t.Fatalf("claimTTL() = %v, want default", got)
	}
	s.deps.Config.Governor.Claims.TTLS = 60
	if got := hub.claimTTL(); got != time.Minute {
		t.Fatalf("claimTTL() = %v, want 1m", got)
	}
}
