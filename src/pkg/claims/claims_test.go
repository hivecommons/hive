package claims

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestComments(t *testing.T) {
	c := Claim{Repo: "o/r", Issue: 4, Holder: "alice", Kind: KindHuman, Hive: "h1", Session: "laptop",
		ExpiresAt: time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)}
	got := ClaimComment(c)
	for _, want := range []string{"<!-- hive:claim who=alice kind=human session=laptop hive=h1 until=2026-09-22T16:00:00Z -->", "🔒 Claimed by @alice (human on hive h1) until 2026-09-22 16:00 UTC"} {
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
	if !strings.Contains(got, "reason=lease_revoked -->") || !strings.Contains(got, "🔓 Claim by @relay-bot released") {
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
