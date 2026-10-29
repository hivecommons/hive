package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every writer that moves a contributor's trust tier must leave a
// TierHistory entry on the profile and a contributor-tier-changed audit
// event naming who, when, from what and why — the question "who changed
// @login to trusted?" was unanswerable before (registry held only the bare
// tier; audit.jsonl had no tier event).

func TestApplyContributorTierChangeRecordsProvenanceAndSkipsNoOps(t *testing.T) {
	now := time.Date(2026, 10, 27, 13, 59, 0, 0, time.UTC)
	p := &ContributorProfile{GitHubUsername: "thatguywob", TrustTier: contributorTierContributor}

	ch, changed := applyContributorTierChange(p, contributorTierTrusted, "clubanderson", "20+ verified PRs", tierChangeSourceOperator, now)
	if !changed {
		t.Fatal("contributor -> trusted must be a change")
	}
	if p.TrustTier != contributorTierTrusted || len(p.TierHistory) != 1 {
		t.Fatalf("profile after change: tier=%q history=%d", p.TrustTier, len(p.TierHistory))
	}
	want := ContributorTierChange{Tier: "trusted", Prev: "contributor", At: "2026-10-27T13:59:00Z", By: "clubanderson", Reason: "20+ verified PRs", Source: "operator"}
	if ch != want || p.TierHistory[0] != want {
		t.Fatalf("entry = %+v, want %+v", ch, want)
	}

	// Writing the tier the profile already holds is a no-op: no history
	// entry, no tier touch.
	if _, changed := applyContributorTierChange(p, "trusted", "someone-else", "again", tierChangeSourceOperator, now.Add(time.Hour)); changed {
		t.Fatal("same-tier write must not be recorded")
	}
	if _, changed := applyContributorTierChange(p, " Trusted ", "someone-else", "again", tierChangeSourceOperator, now.Add(time.Hour)); changed {
		t.Fatal("same-tier write (case/space variant) must not be recorded")
	}
	if len(p.TierHistory) != 1 {
		t.Fatalf("history grew on a no-op: %+v", p.TierHistory)
	}

	cur, ok := currentTierChange(p)
	if !ok || cur != want {
		t.Fatalf("currentTierChange = %+v/%v", cur, ok)
	}
	if _, ok := currentTierChange(&ContributorProfile{TrustTier: "trusted"}); ok {
		t.Fatal("legacy profile without history must report no provenance")
	}
}

func TestApplyContributorTierChangeBoundsHistoryAndReason(t *testing.T) {
	p := &ContributorProfile{TrustTier: contributorTierNewcomer}
	now := time.Now()
	tiers := []string{contributorTierContributor, contributorTierNewcomer}
	for i := 0; i < contributorTierHistoryMax+7; i++ {
		applyContributorTierChange(p, tiers[i%2], "op", strings.Repeat("x", contributorTierReasonMax+50), tierChangeSourceOperator, now)
	}
	if len(p.TierHistory) != contributorTierHistoryMax {
		t.Fatalf("history len = %d, want capped at %d", len(p.TierHistory), contributorTierHistoryMax)
	}
	if got := len(p.TierHistory[0].Reason); got != contributorTierReasonMax {
		t.Fatalf("reason len = %d, want %d", got, contributorTierReasonMax)
	}
	// The newest entry is the last one and matches the tier held now.
	if last := p.TierHistory[len(p.TierHistory)-1]; last.Tier != p.TrustTier {
		t.Fatalf("last entry %+v does not match tier %q", last, p.TrustTier)
	}
}

func TestRegistrationSeedsTierHistory(t *testing.T) {
	setupContributeEnv(t)
	p, _ := createContributorProfile("newbie")
	saved, err := loadContributorProfile(p.GitHubUsername)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TrustTier != contributorTierNewcomer || len(saved.TierHistory) != 1 {
		t.Fatalf("registered profile: tier=%q history=%+v", saved.TrustTier, saved.TierHistory)
	}
	if e := saved.TierHistory[0]; e.Tier != "newcomer" || e.Prev != "" || e.By != tierChangeByRegistration || e.Source != tierChangeSourceRegistration {
		t.Fatalf("registration entry = %+v", e)
	}
}

func readAuditActions(t *testing.T, s *Server, action string) []AuditEntry {
	t.Helper()
	var out []AuditEntry
	for _, e := range s.audit.Recent(100) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestOperatorTrustChangeWritesHistoryAndAudit(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/contributors/alice/trust", strings.NewReader(body))
		req.SetPathValue("id", "alice")
		req.Header.Set("X-Hive-User", "clubanderson")
		markOwnerRequest(req)
		w := httptest.NewRecorder()
		s.handleContributorTrust(w, req)
		return w
	}

	if w := put(`{"tier":"trusted","reason":"20 clean PRs"}`); w.Code != http.StatusOK {
		t.Fatalf("PUT trust = %d %s", w.Code, w.Body.String())
	}
	p := findContributor("alice")
	if p == nil || p.TrustTier != "trusted" || len(p.TierHistory) != 1 {
		t.Fatalf("after PUT: %+v", p)
	}
	e := p.TierHistory[0]
	if e.Prev != "contributor" || e.By != "clubanderson" || e.Reason != "20 clean PRs" || e.Source != tierChangeSourceOperator || e.At == "" {
		t.Fatalf("history entry = %+v", e)
	}
	events := readAuditActions(t, s, auditActionContributorTierChanged)
	if len(events) != 1 {
		t.Fatalf("audit events = %+v, want exactly one", events)
	}
	if events[0].User != "clubanderson" || events[0].Agent != "c-alice" {
		t.Fatalf("audit event = %+v", events[0])
	}
	for _, want := range []string{"username=alice", "tier=trusted", "prev=contributor", "by=clubanderson", "reason=20 clean PRs", "source=operator"} {
		if !strings.Contains(events[0].Detail, want) {
			t.Fatalf("audit detail %q missing %q", events[0].Detail, want)
		}
	}

	// Same tier again: 200, nothing recorded.
	if w := put(`{"tier":"trusted"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"unchanged":true`) {
		t.Fatalf("no-op PUT = %d %s", w.Code, w.Body.String())
	}
	if p := findContributor("alice"); len(p.TierHistory) != 1 {
		t.Fatalf("no-op PUT grew history: %+v", p.TierHistory)
	}
	if got := readAuditActions(t, s, auditActionContributorTierChanged); len(got) != 1 {
		t.Fatalf("no-op PUT wrote audit: %+v", got)
	}

	// GET /api/contributors exposes the history.
	rec := doOwnerGet(s, "/api/contributors")
	var listed struct {
		Contributors []ContributorProfile `json:"contributors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}
	if len(listed.Contributors) != 1 || len(listed.Contributors[0].TierHistory) != 1 || listed.Contributors[0].TierHistory[0].By != "clubanderson" {
		t.Fatalf("GET /api/contributors = %s", rec.Body.String())
	}
}

func TestOperatorRevokeWritesHistoryAndAudit(t *testing.T) {
	setupContributeEnv(t)
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "trusted"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	req := httptest.NewRequest(http.MethodPost, "/api/contributors/bob/revoke", nil)
	req.SetPathValue("id", "bob")
	req.Header.Set("X-Hive-User", "clubanderson")
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	s.handleContributorRevoke(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", w.Code, w.Body.String())
	}
	p := findContributor("bob")
	if p == nil || p.TrustTier != "revoked" || len(p.TierHistory) != 1 || p.TierHistory[0].Prev != "trusted" || p.TierHistory[0].By != "clubanderson" {
		t.Fatalf("after revoke: %+v", p)
	}
	if got := readAuditActions(t, s, auditActionContributorTierChanged); len(got) != 1 || !strings.Contains(got[0].Detail, "tier=revoked") {
		t.Fatalf("audit = %+v", got)
	}
}

func TestAutoPromotionWritesHistoryAndAudit(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	h := NewContributeWSHub(slog.Default(), s)
	profile := &ContributorProfile{GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "newcomer", TasksWithPR: contributorAutoPromoteAt - 1}
	if err := saveContributorProfile(profile); err != nil {
		t.Fatal(err)
	}
	c := &ContributorConnection{profile: profile}

	var promotion ContributorTierChange
	user, promoted, err := saveConnectionContributorProfile(c, func(p *ContributorProfile) bool {
		p.TasksWithPR++
		if p.TrustTier == contributorTierNewcomer && p.TasksWithPR >= contributorAutoPromoteAt {
			ch, changed := applyContributorTierChange(p, contributorTierContributor, tierChangeByAutoPromotion, "threshold", tierChangeSourceAutoPromotion, time.Now())
			promotion = ch
			return changed
		}
		return false
	})
	if err != nil || !promoted || user != "carol" {
		t.Fatalf("save: user=%q promoted=%v err=%v", user, promoted, err)
	}
	h.auditTierChange(user, promotion)

	saved, err := loadContributorProfile("carol")
	if err != nil {
		t.Fatal(err)
	}
	if saved.TrustTier != "contributor" || len(saved.TierHistory) != 1 || saved.TierHistory[0].By != tierChangeByAutoPromotion || saved.TierHistory[0].Source != tierChangeSourceAutoPromotion {
		t.Fatalf("saved = %+v", saved)
	}
	// The live connection's copy carries the history too.
	if len(c.profile.TierHistory) != 1 {
		t.Fatalf("connection profile history = %+v", c.profile.TierHistory)
	}
	got := readAuditActions(t, s, auditActionContributorTierChanged)
	if len(got) != 1 || got[0].User != tierChangeByAutoPromotion || !strings.Contains(got[0].Detail, "prev=newcomer") {
		t.Fatalf("audit = %+v", got)
	}
}

func TestSetContributorTrustTierCarriesHistoryToLiveConnections(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	h := NewContributeWSHub(slog.Default(), s)
	disk := &ContributorProfile{GitHubUsername: "dave", ContributorID: "c-dave", TrustTier: "contributor"}
	applyContributorTierChange(disk, "trusted", "clubanderson", "r", tierChangeSourceOperator, time.Now())
	if err := saveContributorProfileCAS(disk, true); err != nil {
		t.Fatal(err)
	}
	live := &ContributorConnection{profile: &ContributorProfile{GitHubUsername: "dave", ContributorID: "c-dave", TrustTier: "contributor"}}
	h.mu.Lock()
	h.connections["conn-1"] = live
	h.mu.Unlock()

	h.SetContributorTrustTier("c-dave", "trusted")
	if live.profile.TrustTier != "trusted" || len(live.profile.TierHistory) != 1 || live.profile.TierHistory[0].By != "clubanderson" {
		t.Fatalf("live profile = %+v", live.profile)
	}
}
