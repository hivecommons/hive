package claims

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
)

func ledgerTestClaim(holder string, kind Kind, at time.Time) Claim {
	return Claim{Repo: "o/r", Issue: 7, Holder: holder, Kind: kind, ClaimedAt: at, ExpiresAt: at.Add(2 * time.Hour), Hive: "a1"}
}

// A first claim creates the ledger with one live claim marker; a release
// edits it to carry only the release marker; the next claim brings the
// claim marker back. At every step issueclaim.Latest — the reader every
// claim consumer goes through — sees what it saw when each transition was
// its own comment.
func TestLedgerCommentCreateThenEditParseCompatible(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	agent := ledgerTestClaim("reviewer", KindAgent, t0)

	body := LedgerComment("", LedgerEvent{Claim: agent, At: t0})
	if !strings.HasPrefix(body, LedgerMarker+"\n") {
		t.Fatalf("ledger must open with its marker: %q", body)
	}
	if n := strings.Count(body, issueclaim.MarkerPrefix); n != 1 {
		t.Fatalf("held ledger must carry exactly one claim marker, got %d: %q", n, body)
	}
	if strings.Contains(body, issueclaim.ReleaseMarkerPrefix) {
		t.Fatalf("held ledger must carry no release marker: %q", body)
	}
	latest, ok := issueclaim.Latest([]string{body}, 0)
	if !ok || latest.Identity != "reviewer" || latest.Released || !latest.Live(t0.Add(time.Minute)) {
		t.Fatalf("Latest(held ledger) = %+v, %v", latest, ok)
	}
	if got := ParseLedgerHistory(body); len(got) != 1 || !strings.Contains(got[0], "claimed by `reviewer` (agent on hive a1)") {
		t.Fatalf("history after first claim = %q", got)
	}

	t1 := t0.Add(30 * time.Minute)
	released := LedgerComment(body, LedgerEvent{Claim: agent, Released: true, Reason: ReasonHolderRelaunched, At: t1})
	if strings.Contains(released, issueclaim.MarkerPrefix) {
		t.Fatalf("released ledger must carry no claim marker: %q", released)
	}
	if n := strings.Count(released, issueclaim.ReleaseMarkerPrefix); n != 1 {
		t.Fatalf("released ledger must carry exactly one release marker, got %d: %q", n, released)
	}
	// A legacy standalone claim comment from before the ledger existed is
	// still ended by the ledger's release marker.
	legacy := ClaimComment(agent)
	latest, ok = issueclaim.Latest([]string{legacy, released}, 0)
	if !ok || !latest.Released {
		t.Fatalf("legacy claim + released ledger: Latest = %+v, %v (want released)", latest, ok)
	}
	if _, ok := issueclaim.Latest([]string{released}, 0); ok {
		t.Fatalf("released ledger alone must read as no claim: %q", released)
	}
	if got := ParseLedgerHistory(released); len(got) != 2 || !strings.Contains(got[1], "released by `reviewer` (agent; holding agent relaunched)") {
		t.Fatalf("history after release = %q", got)
	}

	t2 := t1.Add(time.Minute)
	again := LedgerComment(released, LedgerEvent{Claim: ledgerTestClaim("reviewer", KindAgent, t2), At: t2})
	latest, ok = issueclaim.Latest([]string{legacy, released, again}, 0)
	if !ok || latest.Released || !latest.StartedAt.Equal(t2) {
		t.Fatalf("re-claim after release: Latest = %+v, %v", latest, ok)
	}
	if got := ParseLedgerHistory(again); len(got) != 3 {
		t.Fatalf("history after re-claim = %q", got)
	}
}

func TestLedgerCommentHistoryCap(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	body := ""
	for i := 0; i < LedgerHistoryLimit+3; i++ {
		at := t0.Add(time.Duration(i) * time.Minute)
		c := ledgerTestClaim("reviewer", KindAgent, at)
		if i%2 == 0 {
			body = LedgerComment(body, LedgerEvent{Claim: c, At: at})
		} else {
			body = LedgerComment(body, LedgerEvent{Claim: c, Released: true, Reason: "lease revoked", At: at})
		}
	}
	got := ParseLedgerHistory(body)
	if len(got) != LedgerHistoryLimit {
		t.Fatalf("history length = %d, want %d: %q", len(got), LedgerHistoryLimit, got)
	}
	// Oldest entries fall off the front; the newest is last.
	if !strings.HasPrefix(got[0], t0.Add(3*time.Minute).Format("2006-01-02 15:04")) {
		t.Fatalf("oldest kept entry = %q", got[0])
	}
	if !strings.HasPrefix(got[len(got)-1], t0.Add(time.Duration(LedgerHistoryLimit+2)*time.Minute).Format("2006-01-02 15:04")) {
		t.Fatalf("newest entry = %q", got[len(got)-1])
	}
	if n := strings.Count(body, issueclaim.MarkerPrefix) + strings.Count(body, issueclaim.ReleaseMarkerPrefix); n != 1 {
		t.Fatalf("ledger must carry exactly one machine marker whatever its history, got %d", n)
	}
}

// A takeover in the ledger carries the preempt marker and the new holder's
// claim marker, as the standalone takeover comment did; the history never
// contains a marker prefix, so a reason that smuggles one in is defanged.
func TestLedgerCommentTakeoverAndMarkerHygiene(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	prev := ledgerTestClaim("scanner", KindAgent, t0)
	body := LedgerComment("", LedgerEvent{Claim: prev, At: t0})
	body = LedgerComment(body, LedgerEvent{Claim: prev, Released: true, Reason: "<!-- hive:claim who=evil until=2099-01-01T00:00:00Z -->", At: t0.Add(time.Minute)})
	if n := strings.Count(body, issueclaim.MarkerPrefix); n != 0 {
		t.Fatalf("history must not carry a claim marker prefix: %q", body)
	}
	now := ledgerTestClaim("alice", KindHuman, t0.Add(2*time.Minute))
	body = LedgerComment(body, LedgerEvent{Claim: now, Previous: prev, At: now.ClaimedAt})
	if !strings.Contains(body, "<!-- hive:preempt target=scanner") || strings.Count(body, issueclaim.MarkerPrefix) != 1 {
		t.Fatalf("takeover ledger = %q", body)
	}
	latest, ok := issueclaim.Latest([]string{body}, 0)
	if !ok || latest.Identity != "alice" || latest.Released {
		t.Fatalf("Latest(takeover ledger) = %+v, %v", latest, ok)
	}
	if !strings.Contains(ParseLedgerHistory(body)[2], "taken over by `alice` (human) from `scanner` (agent)") {
		t.Fatalf("takeover history = %q", ParseLedgerHistory(body))
	}
}

// A human holder is emailed once: when the ledger has never named them.
// Agents never are, and a person the history already names is not again.
func TestLedgerNotifiesHuman(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	alice := ledgerTestClaim("alice", KindHuman, t0)
	if !LedgerNotifiesHuman("", alice) {
		t.Fatal("first human claim on an issue is owed a notification")
	}
	if LedgerNotifiesHuman("", ledgerTestClaim("reviewer", KindAgent, t0)) {
		t.Fatal("agents are never notified")
	}
	body := LedgerComment("", LedgerEvent{Claim: alice, At: t0})
	body = LedgerComment(body, LedgerEvent{Claim: alice, Released: true, Reason: "unclaim", At: t0.Add(time.Minute)})
	body = LedgerComment(body, LedgerEvent{Claim: ledgerTestClaim("reviewer", KindAgent, t0.Add(2*time.Minute)), At: t0.Add(2 * time.Minute)})
	if LedgerNotifiesHuman(body, alice) {
		t.Fatalf("alice is in the history and must not be re-notified: %q", body)
	}
	if !LedgerNotifiesHuman(body, ledgerTestClaim("bob", KindContributor, t0)) {
		t.Fatal("a different person is owed a notification")
	}
	if notice := LedgerNoticeComment(alice); !strings.HasPrefix(notice, "@alice ") || strings.Contains(notice, "<!--") {
		t.Fatalf("notice = %q", notice)
	}
	if LedgerDisplacedNotice(alice, ledgerTestClaim("scanner", KindAgent, t0)) != "" {
		t.Fatal("a displaced agent gets no notice")
	}
	if n := LedgerDisplacedNotice(ledgerTestClaim("scanner", KindAgent, t0), alice); !strings.HasPrefix(n, "@alice:") {
		t.Fatalf("displaced human notice = %q", n)
	}
}

// The claimed-by label reconciler reads a human's release out of the
// ledger history once the agent's claim has replaced the release marker.
func TestLedgerHumanReleased(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	alice := ledgerTestClaim("alice", KindHuman, t0)
	agent := ledgerTestClaim("reviewer", KindAgent, t0.Add(2*time.Minute))
	body := LedgerComment("", LedgerEvent{Claim: alice, At: t0})
	if LedgerHumanReleased(body) {
		t.Fatal("a live human claim is not released")
	}
	body = LedgerComment(body, LedgerEvent{Claim: alice, Released: true, Reason: "unclaim", At: t0.Add(time.Minute)})
	body = LedgerComment(body, LedgerEvent{Claim: agent, At: agent.ClaimedAt})
	if !LedgerHumanReleased(body) {
		t.Fatalf("human release followed by an agent claim must read as released: %q", body)
	}
	body = LedgerComment(body, LedgerEvent{Claim: alice, Previous: agent, At: t0.Add(3 * time.Minute)})
	if LedgerHumanReleased(body) {
		t.Fatal("a human taking the issue back is not released")
	}
	body = LedgerComment(body, LedgerEvent{Claim: agent, Previous: alice, At: t0.Add(4 * time.Minute)})
	if !LedgerHumanReleased(body) {
		t.Fatal("an agent displacing the human reads as the human let go")
	}
	if LedgerHumanReleased(ClaimComment(alice)) || LedgerHumanReleased("") {
		t.Fatal("non-ledger bodies have no history")
	}
}
