package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
)

// Issue claims (hivecommons/hive#8380): the dashboard seam between the
// worker-claim ledger and the relay. These pin (1) selectTask honouring a
// human's claim, (2) the auto-claim/auto-release that travels with a lease,
// (3) a takeover yanking ONLY the displaced session and handing it other
// work, and (4) the /api/claims routes' auth + outcome mapping.

func claimsHub(t *testing.T) (*ContributeWSHub, *Server, *claims.Ledger) {
	t.Helper()
	hub, s := covK2Hub(t)
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	s.deps.IssueClaims = l
	s.contributeHub = hub
	s.InstallClaimHooks(claims.Hooks{})
	return hub, s, l
}

func TestClaims_SelectTaskSkipsHumanClaimedIssue(t *testing.T) {
	hub, s, l := claimsHub(t)
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "alice", Kind: claims.KindHuman}); err != nil {
		t.Fatal(err)
	}
	setStatusIssues(s,
		intgIssue(1, "alice is on this", "someone", nil),
		intgIssue(2, "free", "someone", nil),
	)
	c := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "worker", ContributorID: "c-worker", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-worker"] = c
	hub.mu.Unlock()

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_assign" || msg.Number != 2 {
		t.Fatalf("expected #2 (the unclaimed one), got %+v", msg)
	}
	// The assignment auto-recorded a contributor claim on #2 …
	got, ok := l.Lookup("myorg/repo1", 2)
	if !ok || got.Kind != claims.KindContributor || got.Holder != "worker" || got.HolderID != "c-worker" {
		t.Fatalf("contributor claim not recorded: %+v ok=%v", got, ok)
	}
	// … and revoking the lease releases it again.
	hub.revokeLease("c-worker", msg.TaskID)
	if _, ok := l.Lookup("myorg/repo1", 2); ok {
		t.Fatal("claim survived lease revoke")
	}
	if _, ok := l.Lookup("myorg/repo1", 1); !ok {
		t.Fatal("alice's claim was disturbed by another identity's revoke")
	}
}

func TestClaims_TakeoverPreemptsOnlyTheDisplacedSession(t *testing.T) {
	hub, s, l := claimsHub(t)
	setStatusIssues(s,
		intgIssue(1, "held by clanker", "someone", nil),
		intgIssue(2, "other work", "someone", nil),
		intgIssue(3, "sibling's work", "someone", nil),
	)
	profile := &ContributorProfile{GitHubUsername: "worker", ContributorID: "c-worker", TrustTier: "contributor"}
	held := &ContributorConnection{
		profile: profile, session: "a",
		currentTask:    &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 1},
		currentTaskGen: 7, lastLeaseRenew: time.Now(), lastPong: time.Now(),
	}
	sibling := &ContributorConnection{
		profile: profile, session: "b",
		currentTask:    &WSTaskAssign{TaskID: "t-3", Repo: "myorg/repo1", Number: 3},
		currentTaskGen: 9, lastLeaseRenew: time.Now(), lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-a"] = held
	hub.connections["conn-b"] = sibling
	hub.mu.Unlock()
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "worker", HolderID: identityOf(held), Kind: claims.KindContributor, Session: "a"}); err != nil {
		t.Fatal(err)
	}

	res, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "alice", Kind: claims.KindHuman})
	if err != nil || res.Outcome != claims.OutcomeTakenOver {
		t.Fatalf("takeover outcome=%s err=%v", res.Outcome, err)
	}

	held.mu.Lock()
	cur, gen := held.currentTask, held.currentTaskGen
	held.mu.Unlock()
	if cur == nil || cur.Number == 1 {
		t.Fatalf("displaced session not moved off #1: %+v", cur)
	}
	if cur.Number != 2 {
		t.Fatalf("displaced session reassigned to #%d, want #2", cur.Number)
	}
	if gen == 7 {
		t.Fatal("assignment generation not bumped — stale completion would not be fenced")
	}
	sibling.mu.Lock()
	sib := sibling.currentTask
	sibling.mu.Unlock()
	if sib == nil || sib.Number != 3 {
		t.Fatalf("sibling session was disturbed: %+v", sib)
	}
	if now, _ := l.Lookup("myorg/repo1", 1); now.Holder != "alice" {
		t.Fatalf("ledger holder=%s want alice", now.Holder)
	}
}

func TestClaims_APIRoutes(t *testing.T) {
	_, s, l := claimsHub(t)
	do := func(method, path, body string, hdr map[string]string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	asAlice := map[string]string{"X-Hive-User": "alice"}
	asOwnerToken := map[string]string{"X-Hive-Role": "owner", ownerRoleVerifiedHeader: "true"}

	if code, _ := do(http.MethodPost, "/api/claims/o/r/5", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous claim code=%d want 401", code)
	}
	if code, _ := do(http.MethodPost, "/api/claims/o/r/zero", "", asAlice); code != http.StatusBadRequest {
		t.Fatalf("bad path code=%d want 400", code)
	}
	code, out := do(http.MethodPost, "/api/claims/o/r/5", `{"ttl_s":3600}`, asAlice)
	if code != http.StatusOK || out["outcome"] != "claimed" {
		t.Fatalf("claim: code=%d out=%v", code, out)
	}
	code, out = do(http.MethodGet, "/api/claims/o/r/5", "", nil)
	if code != http.StatusOK || out["held"] != true {
		t.Fatalf("get: code=%d out=%v", code, out)
	}
	// Same rank, different human → held (409) unless forced.
	code, out = do(http.MethodPost, "/api/claims/o/r/5", "", map[string]string{"X-Hive-User": "bob"})
	if code != http.StatusConflict || out["outcome"] != "held" {
		t.Fatalf("same-rank: code=%d out=%v", code, out)
	}
	code, out = do(http.MethodPost, "/api/claims/o/r/5", `{"force":true}`, map[string]string{"X-Hive-User": "bob"})
	if code != http.StatusOK || out["outcome"] != "taken_over" {
		t.Fatalf("forced: code=%d out=%v", code, out)
	}
	// alice cannot release bob's claim …
	if code, _ = do(http.MethodDelete, "/api/claims/o/r/5", "", asAlice); code != http.StatusConflict {
		t.Fatalf("stranger release code=%d want 409", code)
	}
	// … but an owner token with force can.
	code, out = do(http.MethodDelete, "/api/claims/o/r/5", `{"force":true}`, asOwnerToken)
	if code != http.StatusOK || out["released"] != true {
		t.Fatalf("owner release: code=%d out=%v", code, out)
	}
	if _, ok := l.Lookup("o/r", 5); ok {
		t.Fatal("claim survived owner release")
	}
	code, out = do(http.MethodGet, "/api/claims", "", nil)
	if code != http.StatusOK || out["enabled"] != true {
		t.Fatalf("list: code=%d out=%v", code, out)
	}

	// Feature off → routes answer honestly, never 500.
	s.deps.IssueClaims = nil
	if code, out := do(http.MethodGet, "/api/claims", "", nil); code != http.StatusOK || out["enabled"] != false {
		t.Fatalf("disabled list: code=%d out=%v", code, out)
	}
	if code, _ := do(http.MethodPost, "/api/claims/o/r/5", "", asAlice); code != http.StatusNotFound {
		t.Fatalf("disabled claim code=%d want 404", code)
	}
}

// A relay whose socket is down has no connection to yank, but its lease would
// otherwise let it RESUME the item it lost on reconnect (#4260 resumability).
// Takeover must revoke that lease too.
func TestClaims_TakeoverRevokesDisconnectedRelayLease(t *testing.T) {
	hub, _, l := claimsHub(t)
	const identity = "c-worker#a"
	if err := hub.recordLease(identity, "t-1", "myorg/repo1", 1, "contributor", 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "worker", HolderID: identity, Kind: claims.KindContributor, Session: "a"}); err != nil {
		t.Fatal(err)
	}
	if hub.lookupLease(identity, "t-1", "myorg/repo1", 1, 3, time.Now()) == nil {
		t.Fatal("precondition: lease not recorded")
	}

	res, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "alice", Kind: claims.KindHuman})
	if err != nil || res.Outcome != claims.OutcomeTakenOver {
		t.Fatalf("takeover outcome=%s err=%v", res.Outcome, err)
	}
	if hub.lookupLease(identity, "t-1", "myorg/repo1", 1, 3, time.Now()) != nil {
		t.Fatal("disconnected relay's lease survived the takeover — it could resume #1")
	}
	if now, _ := l.Lookup("myorg/repo1", 1); now.Holder != "alice" {
		t.Fatalf("ledger holder=%s want alice", now.Holder)
	}
}

// The contributor claim is renewed together with the lease so a long task does
// not silently lose its claim after the 30m claim TTL.
func TestClaims_RenewLeaseRenewsClaim(t *testing.T) {
	hub, _, l := claimsHub(t)
	const identity = "c-worker#a"
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	now := base
	l.SetNow(func() time.Time { return now })
	if err := hub.recordLease(identity, "t-1", "myorg/repo1", 1, "contributor", 3, now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Claim(claims.Request{Repo: "myorg/repo1", Issue: 1, Holder: "worker", HolderID: identity, Kind: claims.KindContributor, Session: "a"}); err != nil {
		t.Fatal(err)
	}
	first, _ := l.Lookup("myorg/repo1", 1)
	now = now.Add(10 * time.Minute)
	if err := hub.renewLease(identity, "t-1", now); err != nil {
		t.Fatal(err)
	}
	after, ok := l.Lookup("myorg/repo1", 1)
	if !ok || !after.ExpiresAt.After(first.ExpiresAt) || after.HolderID != identity {
		t.Fatalf("claim not renewed with lease: before=%v after=%+v", first.ExpiresAt, after)
	}
}
