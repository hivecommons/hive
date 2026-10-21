package claims

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
)

func newTestLedger(t *testing.T, hooks Hooks) (*Ledger, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l, err := New(filepath.Join(t.TempDir(), "claims.json"), DefaultPolicy(), hooks)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.SetNow(func() time.Time { return now })
	l.SetHive("test-hive")
	return l, &now
}

func TestClaimFreeIssue(t *testing.T) {
	var claimed []Claim
	l, now := newTestLedger(t, Hooks{OnClaimed: func(c Claim, _ Outcome) { claimed = append(claimed, c) }})
	res, err := l.Claim(Request{Repo: "o/r", Issue: 7, Holder: "alice", Kind: KindHuman})
	if err != nil || res.Outcome != OutcomeClaimed {
		t.Fatalf("outcome=%s err=%v", res.Outcome, err)
	}
	if res.Claim.ExpiresAt != now.Add(DefaultHumanTTL) {
		t.Fatalf("expires=%v want +%v", res.Claim.ExpiresAt, DefaultHumanTTL)
	}
	if len(claimed) != 1 || claimed[0].Hive != "test-hive" {
		t.Fatalf("OnClaimed=%+v", claimed)
	}
	if _, ok := l.Lookup("o/r", 7); !ok {
		t.Fatal("Lookup after claim = miss")
	}
}

func TestClaimRenewSameHolder(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 7, Holder: "alice", Kind: KindHuman})
	*now = now.Add(time.Hour)
	res, _ := l.Claim(Request{Repo: "o/r", Issue: 7, Holder: "alice", Kind: KindHuman})
	if res.Outcome != OutcomeRenewed {
		t.Fatalf("outcome=%s want renewed", res.Outcome)
	}
	if res.Claim.ExpiresAt != now.Add(DefaultHumanTTL) {
		t.Fatalf("renew did not move expiry: %v", res.Claim.ExpiresAt)
	}
}

func TestPrecedenceTable(t *testing.T) {
	cases := []struct {
		name     string
		holder   Kind
		claimant Kind
		force    bool
		want     Outcome
	}{
		{"human over contributor", KindContributor, KindHuman, false, OutcomeTakenOver},
		{"human over agent", KindAgent, KindHuman, false, OutcomeTakenOver},
		{"human over external", KindExternal, KindHuman, false, OutcomeTakenOver},
		{"agent over contributor", KindContributor, KindAgent, false, OutcomeTakenOver},
		{"agent over external", KindExternal, KindAgent, false, OutcomeTakenOver},
		{"contributor over external", KindExternal, KindContributor, false, OutcomeTakenOver},
		{"human vs human warns", KindHuman, KindHuman, false, OutcomeHeld},
		{"human vs human forced", KindHuman, KindHuman, true, OutcomeTakenOver},
		{"contributor vs contributor warns", KindContributor, KindContributor, false, OutcomeHeld},
		{"contributor vs human refused", KindHuman, KindContributor, false, OutcomeRefused},
		{"contributor vs human refused even forced", KindHuman, KindContributor, true, OutcomeRefused},
		{"external vs agent refused", KindAgent, KindExternal, false, OutcomeRefused},
		{"agent vs human refused", KindHuman, KindAgent, false, OutcomeRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var taken []Claim
			l, _ := newTestLedger(t, Hooks{OnTakenOver: func(_ Claim, prev Claim) { taken = append(taken, prev) }})
			if _, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "first", Kind: tc.holder}); err != nil {
				t.Fatal(err)
			}
			res, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "second", Kind: tc.claimant, Force: tc.force})
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.want {
				t.Fatalf("outcome=%s want %s", res.Outcome, tc.want)
			}
			cur, _ := l.Lookup("o/r", 1)
			switch tc.want {
			case OutcomeTakenOver:
				if cur.Holder != "second" || cur.TakenFrom != "first" || cur.TakenFromKind != tc.holder {
					t.Fatalf("after takeover: %+v", cur)
				}
				if len(taken) != 1 || taken[0].Holder != "first" {
					t.Fatalf("OnTakenOver prev=%+v", taken)
				}
				if tc.force && tc.holder == tc.claimant && !cur.Forced {
					t.Fatal("forced same-rank takeover not marked Forced")
				}
			default:
				if cur.Holder != "first" {
					t.Fatalf("holder changed to %s on %s", cur.Holder, tc.want)
				}
				if res.Previous == nil || res.Previous.Holder != "first" {
					t.Fatalf("Previous=%+v", res.Previous)
				}
				if len(taken) != 0 {
					t.Fatal("OnTakenOver fired without a transfer")
				}
			}
		})
	}
}

func TestSessionsOfOneLoginAreDistinctHolders(t *testing.T) {
	l, _ := newTestLedger(t, Hooks{})
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 3, Holder: "alice", Kind: KindHuman, Session: "laptop"})
	res, _ := l.Claim(Request{Repo: "o/r", Issue: 3, Holder: "alice", Kind: KindHuman, Session: "desktop"})
	if res.Outcome != OutcomeHeld {
		t.Fatalf("second session outcome=%s want held (warn)", res.Outcome)
	}
	res, _ = l.Claim(Request{Repo: "o/r", Issue: 3, Holder: "alice", Kind: KindHuman, Session: "laptop"})
	if res.Outcome != OutcomeRenewed {
		t.Fatalf("same session outcome=%s want renewed", res.Outcome)
	}
}

func TestReleaseRules(t *testing.T) {
	var released []string
	l, _ := newTestLedger(t, Hooks{OnReleased: func(c Claim, reason string) { released = append(released, c.Holder+":"+reason) }})
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 5, Holder: "relay-1", HolderID: "relay-1#s", Kind: KindContributor})

	if _, ok, err := l.Release("o/r", 5, "someone", KindContributor, "x"); ok || err == nil {
		t.Fatalf("same-rank stranger released: ok=%v err=%v", ok, err)
	}
	if _, ok, err := l.Release("o/r", 5, "bob", KindHuman, "maintainer"); !ok || err != nil {
		t.Fatalf("human could not release contributor claim: ok=%v err=%v", ok, err)
	}
	if _, ok := l.Lookup("o/r", 5); ok {
		t.Fatal("claim survived release")
	}
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 6, Holder: "relay-1", HolderID: "relay-1#s", Kind: KindContributor})
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 8, Holder: "relay-2", HolderID: "relay-2#s", Kind: KindContributor})
	out := l.ReleaseByHolderID("relay-1#s", "lease revoked")
	if len(out) != 1 || out[0].Issue != 6 {
		t.Fatalf("ReleaseByHolderID=%+v", out)
	}
	if _, ok := l.Lookup("o/r", 8); !ok {
		t.Fatal("other holder's claim was released")
	}
	if len(released) != 2 || released[0] != "relay-1:maintainer" || released[1] != "relay-1:lease revoked" {
		t.Fatalf("OnReleased=%v", released)
	}
}

func TestExpiryAndHeldKeys(t *testing.T) {
	var released int
	l, now := newTestLedger(t, Hooks{OnReleased: func(Claim, string) { released++ }})
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "relay", HolderID: "relay#s", Kind: KindContributor})
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 2, Holder: "alice", Kind: KindHuman})

	keys := l.HeldKeys("relay#s")
	if keys["o/r#1"] || !keys["o/r#2"] {
		t.Fatalf("HeldKeys(except relay)=%v", keys)
	}
	*now = now.Add(DefaultContributorTTL + time.Second)
	if _, ok := l.Lookup("o/r", 1); ok {
		t.Fatal("expired contributor claim still visible")
	}
	if n := l.Expire(); n != 1 || released != 1 {
		t.Fatalf("Expire=%d released=%d", n, released)
	}
	if got := len(l.List()); got != 1 {
		t.Fatalf("List=%d want 1", got)
	}
	// A free issue after expiry can be claimed by a lower rank again.
	res, _ := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "relay-b", Kind: KindContributor})
	if res.Outcome != OutcomeClaimed {
		t.Fatalf("post-expiry outcome=%s", res.Outcome)
	}
}

func TestTTLOverrideAndClamp(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	res, _ := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "a", Kind: KindHuman, TTL: 6 * time.Hour})
	if res.Claim.ExpiresAt != now.Add(6*time.Hour) {
		t.Fatalf("explicit TTL ignored: %v", res.Claim.ExpiresAt)
	}
	res, _ = l.Claim(Request{Repo: "o/r", Issue: 2, Holder: "a", Kind: KindHuman, TTL: 99 * time.Hour})
	if res.Claim.ExpiresAt != now.Add(DefaultMaxTTL) {
		t.Fatalf("TTL not clamped: %v", res.Claim.ExpiresAt)
	}
}

func TestPolicyWithOverridesAndFallbackTTL(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	policy := PolicyWith(5*time.Hour, 3*time.Hour, time.Hour, 6*time.Hour)
	l, err := New("", policy, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	l.SetNow(func() time.Time { return now })

	cases := []struct {
		name string
		req  Request
		want time.Duration
	}{
		{"human override", Request{Repo: "o/r", Issue: 1, Holder: "human", Kind: KindHuman}, 5 * time.Hour},
		{"agent override", Request{Repo: "o/r", Issue: 2, Holder: "agent", Kind: KindAgent}, 3 * time.Hour},
		{"contributor override", Request{Repo: "o/r", Issue: 3, Holder: "relay", Kind: KindContributor}, time.Hour},
		{"default external retained", Request{Repo: "o/r", Issue: 4, Holder: "bot", Kind: KindExternal}, DefaultExternalTTL},
		{"request clamped", Request{Repo: "o/r", Issue: 5, Holder: "human", Kind: KindHuman, TTL: 12 * time.Hour}, 6 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := l.Claim(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Claim.ExpiresAt.Sub(now); got != tc.want {
				t.Fatalf("ttl=%v want %v", got, tc.want)
			}
		})
	}

	fallback, err := New("", Policy{TTL: map[Kind]time.Duration{}, Default: 90 * time.Minute}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	fallback.SetNow(func() time.Time { return now })
	res, err := fallback.Claim(Request{Repo: "o/r", Issue: 6, Holder: "bot", Kind: KindExternal})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Claim.ExpiresAt.Sub(now); got != 90*time.Minute {
		t.Fatalf("fallback ttl=%v want 90m", got)
	}
}

func TestSetHooksAndForceRelease(t *testing.T) {
	l, _ := newTestLedger(t, Hooks{})
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "alice", Kind: KindHuman}); err != nil {
		t.Fatal(err)
	}

	var released []string
	l.SetHooks(Hooks{OnReleased: func(c Claim, reason string) {
		released = append(released, c.Holder+":"+reason)
	}})
	if c, ok := l.ForceRelease(" o/r ", 1, "operator"); !ok || c.Holder != "alice" {
		t.Fatalf("ForceRelease=%+v ok=%v", c, ok)
	}
	if len(released) != 1 || released[0] != "alice:operator" {
		t.Fatalf("OnReleased=%v", released)
	}
	if _, ok := l.ForceRelease("o/r", 99, "missing"); ok {
		t.Fatal("ForceRelease reported missing claim as released")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "claims.json")
	l, err := New(path, DefaultPolicy(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 9, Holder: "alice", Kind: KindHuman, Session: "s1"})
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 10, Holder: "quality", HolderID: "quality", Kind: KindAgent})

	reloaded, err := New(path, DefaultPolicy(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.List()
	if len(got) != 2 {
		t.Fatalf("reloaded=%+v", got)
	}
	byIssue := map[int]Claim{}
	for _, c := range got {
		byIssue[c.Issue] = c
	}
	if byIssue[9].Session != "s1" || byIssue[10].Kind != KindAgent {
		t.Fatalf("reloaded=%+v", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestPersistenceErrorsKeepInMemoryClaimAndFireHooks(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := New(filepath.Join(blocker, "claims.json"), DefaultPolicy(), Hooks{})
	if err == nil {
		t.Fatal("New did not report path under regular file")
	}

	var claimed []Claim
	l.SetHooks(Hooks{OnClaimed: func(c Claim, _ Outcome) { claimed = append(claimed, c) }})
	res, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "alice", Kind: KindHuman})
	if err == nil || res.Outcome != OutcomeClaimed {
		t.Fatalf("Claim outcome=%s err=%v", res.Outcome, err)
	}
	if len(claimed) != 1 || claimed[0].Holder != "alice" {
		t.Fatalf("OnClaimed=%+v", claimed)
	}
	if c, ok := l.Lookup("o/r", 1); !ok || c.Holder != "alice" {
		t.Fatalf("in-memory claim missing after persist error: %+v ok=%v", c, ok)
	}

	dirPath := filepath.Join(t.TempDir(), "claims-dir")
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err = New(dirPath, DefaultPolicy(), Hooks{})
	if err == nil {
		t.Fatal("New did not report directory path")
	}
	if res, err = l.Claim(Request{Repo: "o/r", Issue: 2, Holder: "bob", Kind: KindHuman}); err == nil || res.Outcome != OutcomeClaimed {
		t.Fatalf("Claim with directory path outcome=%s err=%v", res.Outcome, err)
	}
	if c, ok := l.Lookup("o/r", 2); !ok || c.Holder != "bob" {
		t.Fatalf("directory-path claim missing after persist error: %+v ok=%v", c, ok)
	}
}

func TestCorruptFileIsReportedButUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := New(path, DefaultPolicy(), Hooks{})
	if err == nil {
		t.Fatal("corrupt file not reported")
	}
	if res, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "a", Kind: KindHuman}); err != nil || res.Outcome != OutcomeClaimed {
		t.Fatalf("ledger unusable after corrupt load: %v %s", err, res.Outcome)
	}
}

func TestInvalidRequests(t *testing.T) {
	l, _ := newTestLedger(t, Hooks{})
	for _, req := range []Request{
		{Issue: 1, Holder: "a", Kind: KindHuman},
		{Repo: "o/r", Holder: "a", Kind: KindHuman},
		{Repo: "o/r", Issue: 1, Kind: KindHuman},
		{Repo: "o/r", Issue: 1, Holder: "a", Kind: Kind("robot")},
	} {
		if _, err := l.Claim(req); err != ErrInvalid {
			t.Fatalf("%+v: err=%v want ErrInvalid", req, err)
		}
	}
	if k, ok := ParseKind(" Human "); !ok || k != KindHuman {
		t.Fatalf("ParseKind=%q %v", k, ok)
	}
	if _, ok := ParseKind("clanker"); ok {
		t.Fatal("ParseKind accepted unknown kind")
	}
}

func TestNilLedgerIsSafe(t *testing.T) {
	var l *Ledger
	if _, ok := l.Lookup("o/r", 1); ok {
		t.Fatal("nil Lookup hit")
	}
	if l.HeldKeys("") != nil || l.List() != nil || l.Expire() != 0 {
		t.Fatal("nil ledger returned data")
	}
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "a", Kind: KindHuman}); err != ErrInvalid {
		t.Fatalf("nil Claim err=%v", err)
	}
}

func TestClaimCommentNeverShowsStaleExpiry(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l, err := New("", DefaultPolicy(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	l.SetNow(func() time.Time { return now })
	req := Request{Repo: "o/r", Issue: 4, Holder: "scanner", Kind: KindAgent}
	first, err := l.Claim(req)
	if err != nil || first.Outcome != OutcomeClaimed {
		t.Fatalf("first claim: %+v err=%v", first, err)
	}
	posted := ClaimComment(first.Claim)
	now = now.Add(90 * time.Minute)
	renewed, err := l.Claim(req)
	if err != nil || renewed.Outcome != OutcomeRenewed {
		t.Fatalf("renew: %+v err=%v", renewed, err)
	}
	if !renewed.Claim.ExpiresAt.After(first.Claim.ExpiresAt) {
		t.Fatal("renewal did not move the expiry")
	}
	if strings.Contains(posted, " UTC") || strings.Contains(posted, "until ") {
		t.Fatalf("visible comment prints a clock time that goes stale on renewal:\n%s", posted)
	}
	for _, want := range []string{"lasts 2h0m0s without activity", "renews while the work continues"} {
		if !strings.Contains(posted, want) {
			t.Fatalf("comment missing %q:\n%s", want, posted)
		}
	}
}

func TestComments(t *testing.T) {
	c := Claim{Repo: "o/r", Issue: 4, Holder: "alice", Kind: KindHuman, Hive: "h1", Session: "laptop",
		ExpiresAt: time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)}
	got := ClaimComment(c)
	for _, want := range []string{"<!-- hive:claim who=alice kind=human session=laptop hive=h1 until=2026-09-22T16:00:00Z -->", "🔒 Claimed by @alice (human on hive h1)."} {
		if !strings.Contains(got, want) {
			t.Fatalf("ClaimComment missing %q:\n%s", want, got)
		}
	}
	prev := Claim{Repo: "o/r", Issue: 4, Holder: "relay-bot", HolderID: "relay-bot#s", Kind: KindContributor}
	got = TakeoverComment(Claim{Repo: "o/r", Issue: 4, Holder: "quality", Kind: KindAgent, ExpiresAt: c.ExpiresAt}, prev)
	for _, want := range []string{"<!-- hive:preempt target=relay-bot target_kind=contributor by=quality by_kind=agent issue=4 -->", "🔁 Claim taken over by agent `quality` (agent)", "@relay-bot: stop work on this item"} {
		if !strings.Contains(got, want) {
			t.Fatalf("TakeoverComment missing %q:\n%s", want, got)
		}
	}
	got = ReleaseComment(prev, "lease --> revoked")
	if !strings.Contains(got, "reason=lease_revoked at=") || !strings.Contains(got, "🔓 Claim by @relay-bot released") {
		t.Fatalf("ReleaseComment:\n%s", got)
	}
	if PreemptedLabel("relay-bot") != "preempted:relay-bot" {
		t.Fatal("PreemptedLabel")
	}
}

func TestLazyExpiryFiresOnReleased(t *testing.T) {
	var released []string
	l, now := newTestLedger(t, Hooks{OnReleased: func(c Claim, reason string) { released = append(released, c.Holder+":"+reason) }})
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "quality", Kind: KindAgent}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 2, Holder: "relay", Kind: KindContributor}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(DefaultAgentTTL + time.Minute) // agent lapsed; contributor lapsed too (30m < 2h)

	// A Claim on an unrelated issue notices the lapses and reports them.
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 3, Holder: "alice", Kind: KindHuman}); err != nil {
		t.Fatal(err)
	}
	if len(released) != 2 || !strings.HasSuffix(released[0], ":expired") || !strings.HasSuffix(released[1], ":expired") {
		t.Fatalf("OnReleased after lazy expiry = %v", released)
	}
	if _, ok := l.Lookup("o/r", 1); ok {
		t.Fatal("expired claim still visible")
	}

	// Release of a missing key still reports whatever lapsed meanwhile.
	released = nil
	*now = now.Add(DefaultHumanTTL + time.Minute)
	if _, ok, err := l.Release("o/r", 99, "nobody", KindHuman, "x"); ok || err != nil {
		t.Fatalf("release missing: ok=%v err=%v", ok, err)
	}
	if len(released) != 1 || released[0] != "alice:expired" {
		t.Fatalf("OnReleased on Release path = %v", released)
	}
}

func TestClaimAfterExpiryDoesNotReleaseSameIssueMirror(t *testing.T) {
	var released []string
	l, now := newTestLedger(t, Hooks{OnReleased: func(c Claim, reason string) {
		released = append(released, c.Key()+":"+reason)
	}})
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "relay", Kind: KindContributor}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 2, Holder: "relay", Kind: KindContributor}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(DefaultContributorTTL + time.Second)
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "alice", Kind: KindHuman}); err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0] != "o/r#2:expired" {
		t.Fatalf("expired release hooks = %v, want only unrelated expired issue", released)
	}
}

// #10981: SetPolicy applies to claims made or renewed after the change and
// leaves a live claim's expiry alone.
func TestSetPolicyAppliesToNewAndRenewedClaims(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	held, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "alice", Kind: KindHuman})
	if err != nil {
		t.Fatal(err)
	}
	l.SetPolicy(PolicyWith(time.Hour, 10*time.Minute, 0, 0))
	if c, ok := l.Lookup("o/r", 1); !ok || c.ExpiresAt != held.Claim.ExpiresAt {
		t.Fatalf("live claim expiry changed: %+v", c)
	}
	res, err := l.Claim(Request{Repo: "o/r", Issue: 2, Holder: "bot", Kind: KindAgent})
	if err != nil || res.Claim.ExpiresAt != now.Add(10*time.Minute) {
		t.Fatalf("new agent claim expires=%v err=%v, want +10m", res.Claim.ExpiresAt, err)
	}
	res, err = l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "alice", Kind: KindHuman})
	if err != nil || res.Outcome != OutcomeRenewed || res.Claim.ExpiresAt != now.Add(time.Hour) {
		t.Fatalf("renewal outcome=%s expires=%v err=%v, want +1h", res.Outcome, res.Claim.ExpiresAt, err)
	}
	res, err = l.Claim(Request{Repo: "o/r", Issue: 3, Holder: "relay", Kind: KindContributor})
	if err != nil || res.Claim.ExpiresAt != now.Add(DefaultContributorTTL) {
		t.Fatalf("contributor expires=%v err=%v, want the default", res.Claim.ExpiresAt, err)
	}
	l.SetPolicy(Policy{})
	res, err = l.Claim(Request{Repo: "o/r", Issue: 4, Holder: "bot", Kind: KindAgent})
	if err != nil || res.Claim.ExpiresAt != now.Add(DefaultAgentTTL) {
		t.Fatalf("empty policy must fall back to defaults: expires=%v err=%v", res.Claim.ExpiresAt, err)
	}
}

// ClaimMarker carries at= when the claim instant is known, so a reader (and
// pkg/issueclaim) can tell when work STARTED, not only when the hold lapses.
func TestClaimMarkerCarriesStartInstant(t *testing.T) {
	started := time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)
	c := Claim{Repo: "o/r", Issue: 4, Holder: "scanner", HolderID: "scanner", Kind: KindAgent, Hive: "h1",
		ClaimedAt: started, ExpiresAt: started.Add(2 * time.Hour)}
	want := "<!-- hive:claim who=scanner kind=agent hive=h1 at=2026-10-14T15:00:00Z until=2026-10-14T17:00:00Z -->"
	if got := ClaimMarker(c); got != want {
		t.Fatalf("ClaimMarker = %q, want %q", got, want)
	}
	if got := ClaimComment(c); !strings.HasPrefix(got, want+"\n") {
		t.Fatalf("ClaimComment must open with the marker:\n%s", got)
	}
	c.ClaimedAt = time.Time{}
	if got := ClaimMarker(c); strings.Contains(got, "at=") {
		t.Fatalf("unknown start must omit at=: %q", got)
	}
}

// A contributor marker carries the holder's trust tier and whether the hub
// had verified the GitHub login, and pkg/issueclaim reads both back. The
// contributor-lane auto-merge tier gates on these fields (#12041).
func TestContributorClaimMarkerTierAndVerifiedRoundTrip(t *testing.T) {
	started := time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)
	c := Claim{Repo: "o/r", Issue: 4, Holder: "alice", HolderID: "c1#s", Kind: KindContributor, Hive: "h1",
		Tier: "trusted", Verified: true, ClaimedAt: started, ExpiresAt: started.Add(2 * time.Hour)}
	want := "<!-- hive:claim who=alice kind=contributor id=c1#s hive=h1 tier=trusted verified=true at=2026-10-14T15:00:00Z until=2026-10-14T17:00:00Z -->"
	if got := ClaimMarker(c); got != want {
		t.Fatalf("ClaimMarker = %q, want %q", got, want)
	}
	parsed, ok := issueclaim.ParseMarker(ClaimComment(c))
	if !ok || parsed.Tier != "trusted" || !parsed.Verified || parsed.Hive != "h1" {
		t.Fatalf("round trip = %+v ok=%v", parsed, ok)
	}

	for name, tier := range map[string]string{"empty": "", "spaces": "trusted verified=true", "marker close": "x-->"} {
		c := Claim{Holder: "bob", Kind: KindContributor, Tier: tier, ExpiresAt: started}
		parsed, ok := issueclaim.ParseMarker(ClaimMarker(c))
		if !ok || parsed.Tier != "newcomer" || parsed.Verified {
			t.Fatalf("%s tier: parsed %+v ok=%v, want newcomer/unverified", name, parsed, ok)
		}
	}

	agent := ClaimMarker(Claim{Holder: "scanner", Kind: KindAgent, Tier: "trusted", ExpiresAt: started})
	if strings.Contains(agent, "tier=") || strings.Contains(agent, "verified=") {
		t.Fatalf("non-contributor marker must not carry tier/verified: %q", agent)
	}
}

func TestLedgerClaimCarriesContributorTier(t *testing.T) {
	l, _ := newTestLedger(t, Hooks{})
	res, err := l.Claim(Request{Repo: "o/r", Issue: 9, Holder: "alice", HolderID: "c1#s", Kind: KindContributor, Tier: "merger", Verified: true})
	if err != nil || res.Outcome != OutcomeClaimed {
		t.Fatalf("claim: %+v err=%v", res, err)
	}
	if res.Claim.Tier != "merger" || !res.Claim.Verified {
		t.Fatalf("ledger claim lost tier/verified: %+v", res.Claim)
	}
}

func TestReleaseAgentClaimsDropsOnlyThatAgentsClaims(t *testing.T) {
	var released []string
	l, _ := newTestLedger(t, Hooks{OnReleased: func(c Claim, reason string) { released = append(released, c.Key()+":"+reason) }})
	for _, req := range []Request{
		{Repo: "o/r", Issue: 1, Holder: "scanner", HolderID: "scanner", Kind: KindAgent},
		{Repo: "o/r", Issue: 2, Holder: "scanner", HolderID: "scanner", Kind: KindAgent},
		{Repo: "o/r", Issue: 3, Holder: "quality", HolderID: "quality", Kind: KindAgent},
		// A human whose login spells the agent's name is not the agent.
		{Repo: "o/r", Issue: 4, Holder: "scanner", HolderID: "scanner", Kind: KindHuman},
	} {
		if _, err := l.Claim(req); err != nil {
			t.Fatalf("Claim %d: %v", req.Issue, err)
		}
	}
	out := l.ReleaseAgentClaims(" scanner ", ReasonHolderRelaunched)
	if len(out) != 2 {
		t.Fatalf("released %d claims, want 2: %+v", len(out), out)
	}
	sort.Strings(released)
	want := []string{"o/r#1:" + ReasonHolderRelaunched, "o/r#2:" + ReasonHolderRelaunched}
	if !reflect.DeepEqual(released, want) {
		t.Fatalf("OnReleased = %v, want %v", released, want)
	}
	for issue, wantHeld := range map[int]bool{1: false, 2: false, 3: true, 4: true} {
		if _, held := l.Lookup("o/r", issue); held != wantHeld {
			t.Fatalf("issue %d held=%v want %v", issue, held, wantHeld)
		}
	}
	if l.ReleaseAgentClaims("", "x") != nil || l.ReleaseAgentClaims("nobody", "x") != nil {
		t.Fatal("no-op releases must return nil")
	}
	var nilLedger *Ledger
	if nilLedger.ReleaseAgentClaims("scanner", "x") != nil {
		t.Fatal("nil ledger must return nil")
	}
	// The release comment carries at= so the forge reader can order it.
	body := releaseCommentAt(out[0], ReasonHolderRelaunched, time.Date(2026, 10, 20, 15, 35, 0, 0, time.UTC))
	if !strings.Contains(body, "<!-- hive:release who=scanner kind=agent reason=holding_agent_relaunched at=2026-10-20T15:35:00Z -->") {
		t.Fatalf("release comment marker:\n%s", body)
	}
	if !strings.Contains(ReleaseComment(out[0], "x"), " at=") {
		t.Fatal("ReleaseComment must stamp at=")
	}
}
