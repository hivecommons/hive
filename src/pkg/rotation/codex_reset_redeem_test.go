package rotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the consent-gated banked-reset redemption controller
// (hivecommons/hive#10598). None of them sleep: provider replies, clocks and
// interleavings are all driven explicitly.

func redeemIntp(n int) *int { return &n }

// redeemReading is a fresh codex reading with a short-term and a weekly window.
func redeemReading(weeklyPct, shortPct int, credits *int) Headroom {
	return Headroom{
		Provider: "openai",
		Limits: []LimitWindow{
			{ID: "codex-5h", Kind: "five_hour", PctRemaining: shortPct},
			{ID: "codex-weekly", Kind: "weekly", PctRemaining: weeklyPct},
		},
		ResetCreditsAvailable: credits,
	}
}

var (
	redeemExhausted = redeemReading(0, 60, redeemIntp(2))
	redeemRecovered = redeemReading(100, 60, redeemIntp(1))
)

// redeemFake is a scripted provider: it counts redemption requests, records
// the idempotency keys they carried, and answers with reply.
type redeemFake struct {
	mu    sync.Mutex
	keys  []string
	reply func(ctx context.Context, key string) (json.RawMessage, error)
}

func (f *redeemFake) consume(ctx context.Context, key string) (json.RawMessage, error) {
	f.mu.Lock()
	f.keys = append(f.keys, key)
	f.mu.Unlock()
	return f.reply(ctx, key)
}

func (f *redeemFake) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func redeemReply(s string) func(context.Context, string) (json.RawMessage, error) {
	return func(context.Context, string) (json.RawMessage, error) { return json.RawMessage(s), nil }
}

// redeemSeq returns readings in order, repeating the last one.
func redeemSeq(hs ...Headroom) func(context.Context) Headroom {
	var mu sync.Mutex
	i := 0
	return func(context.Context) Headroom {
		mu.Lock()
		defer mu.Unlock()
		h := hs[i]
		if i < len(hs)-1 {
			i++
		}
		return h
	}
}

type redeemClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *redeemClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *redeemClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestRedeemer(dir string, fake *redeemFake, clock *redeemClock) *codexResetRedeemer {
	r := newCodexResetRedeemer(dir, "codex", "")
	r.consume = fake.consume
	r.now = clock.now
	var n atomic.Int32
	r.newKey = func() string { return fmt.Sprintf("key-%d", n.Add(1)) }
	return r
}

func newRedeemClock() *redeemClock {
	return &redeemClock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
}

func readRedeemState(t *testing.T, r *codexResetRedeemer) (codexResetRedeemState, bool) {
	t.Helper()
	b, err := os.ReadFile(r.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return codexResetRedeemState{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var st codexResetRedeemState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st, true
}

// redeemProber is a HeadroomSource over a scripted reading sequence.
type redeemProber struct {
	provider string
	next     func(context.Context) Headroom
}

func (p redeemProber) Provider() string                   { return p.provider }
func (p redeemProber) Probe(ctx context.Context) Headroom { return p.next(ctx) }

func TestCodexResetRedeem_DefaultOff(t *testing.T) {
	dir := t.TempDir()
	m, ok := NewContributorBackendReadingPublisher(dir, "", "codex")
	if !ok {
		t.Fatal("codex must get a publisher")
	}
	probes := 0
	m.SetHeadroomSources([]HeadroomSource{redeemProber{provider: "openai", next: func(context.Context) Headroom {
		probes++
		return redeemExhausted
	}}})
	m.probeAll(context.Background())
	if m.codexResetRedeem != nil {
		t.Fatal("the controller must be off unless the contributor opted in")
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1 (no redemption re-read when off)", probes)
	}
	if _, err := os.Stat(newCodexResetRedeemer(dir, "codex", "").statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no attempt state may be written when off: %v", err)
	}

	// Enabling without a publish directory is a no-op.
	off := NewManager(m.cfg)
	off.EnableCodexResetRedeem()
	if off.codexResetRedeem != nil {
		t.Fatal("no publish dir means no controller")
	}
}

func TestCodexResetRedeem_ManagerRedeemsAndPublishesRefreshedReading(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewContributorBackendReadingPublisher(dir, "", "codex")
	m.EnableCodexResetRedeem()
	if m.codexResetRedeem == nil {
		t.Fatal("opt-in must start the controller")
	}
	fake := &redeemFake{reply: redeemReply(`{"status":"reset"}`)}
	m.codexResetRedeem.consume = fake.consume
	m.SetHeadroomSources([]HeadroomSource{redeemProber{provider: "openai", next: redeemSeq(redeemExhausted, redeemExhausted, redeemRecovered)}})

	m.probeAll(context.Background())

	if got := len(fake.calls()); got != 1 {
		t.Fatalf("redemptions = %d, want 1", got)
	}
	b, err := os.ReadFile(ContributorReadingPath(dir, "codex", ""))
	if err != nil {
		t.Fatal(err)
	}
	var reading ContributorReading
	if err := json.Unmarshal(b, &reading); err != nil {
		t.Fatal(err)
	}
	for _, w := range reading.Limits {
		if w.Kind == "weekly" && w.PctRemaining != 100 {
			t.Fatalf("published weekly = %d%%, want the refreshed 100%%", w.PctRemaining)
		}
	}
	if _, ok := readRedeemState(t, m.codexResetRedeem); ok {
		t.Fatal("a recovered pool must not keep an attempt record")
	}
	if got := m.HeadroomFor("openai"); !codexReadingFresh(got) || codexWeeklyExhausted(got) {
		t.Fatalf("stored headroom = %+v, want the refreshed reading", got)
	}
}

func TestCodexResetRedeem_ManagerIgnoresOtherProviders(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewContributorBackendReadingPublisher(dir, "", "codex")
	m.EnableCodexResetRedeem()
	fake := &redeemFake{reply: redeemReply(`"reset"`)}
	m.codexResetRedeem.consume = fake.consume
	h := redeemExhausted
	h.Provider = "anthropic"
	got := m.maybeRedeemCodexReset(context.Background(), redeemProber{provider: "anthropic", next: redeemSeq(h)}, h)
	if len(fake.calls()) != 0 || got.Provider != "anthropic" {
		t.Fatal("only the codex (openai) provider may redeem")
	}
}

// Only a weekly window at 0% with a banked credit triggers. A weekly window
// at or below the reserve, an exhausted short-term window, or no credit hold
// normally and never call the provider or even re-read.
func TestCodexResetRedeem_TriggerIsWeeklyExhaustionWithCreditOnly(t *testing.T) {
	cases := []struct {
		name string
		h    Headroom
	}{
		{"short-term exhaustion only", redeemReading(50, 0, redeemIntp(3))},
		{"reserve only (weekly 5%)", redeemReading(5, 60, redeemIntp(3))},
		{"weekly 1%", redeemReading(1, 60, redeemIntp(3))},
		{"no credit", redeemReading(0, 60, redeemIntp(0))},
		{"credit unknown", redeemReading(0, 60, nil)},
		{"probe failed", func() Headroom { h := redeemExhausted; h.ProbeErr = errors.New("boom"); return h }()},
		{"stale reading", func() Headroom { h := redeemExhausted; h.Stale = true; return h }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &redeemFake{reply: redeemReply(`"reset"`)}
			r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
			rereads := 0
			got := r.evaluate(context.Background(), tc.h, func(context.Context) Headroom { rereads++; return redeemRecovered })
			if len(fake.calls()) != 0 || rereads != 0 {
				t.Fatalf("redemptions=%d rereads=%d, want none", len(fake.calls()), rereads)
			}
			if codexWeeklyExhausted(got) != codexWeeklyExhausted(tc.h) {
				t.Fatal("a hold must publish the original reading")
			}
		})
	}
}

func TestCodexResetRedeem_WeeklyScopedTriggers(t *testing.T) {
	fake := &redeemFake{reply: redeemReply(`"reset"`)}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	h := Headroom{Provider: "openai", Limits: []LimitWindow{{Kind: "weekly_scoped", PctRemaining: 0}}, ResetCreditsAvailable: redeemIntp(1)}
	r.evaluate(context.Background(), h, redeemSeq(h, redeemRecovered))
	if len(fake.calls()) != 1 {
		t.Fatal("an exhausted weekly_scoped window is a weekly window")
	}
}

func TestCodexResetRedeem_SuccessThenRecovery(t *testing.T) {
	for _, outcome := range []string{`{"status":"reset"}`, `{"outcome":"alreadyRedeemed"}`} {
		t.Run(outcome, func(t *testing.T) {
			fake := &redeemFake{reply: redeemReply(outcome)}
			r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
			got := r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted, redeemRecovered))
			if calls := fake.calls(); len(calls) != 1 || calls[0] != "key-1" {
				t.Fatalf("calls = %v, want one with key-1", calls)
			}
			if codexWeeklyExhausted(got) || !codexReadingFresh(got) {
				t.Fatalf("got %+v, want the refreshed clearing reading", got)
			}
			if _, ok := readRedeemState(t, r); ok {
				t.Fatal("recovery must clear the attempt record")
			}
		})
	}
}

// A provider slow to reflect the reset must not drain the bank: once an
// attempt is confirmed, nothing more is redeemed until a fresh reading shows
// the weekly window off 0%.
func TestCodexResetRedeem_RedeemedButStillExhaustedHolds(t *testing.T) {
	fake := &redeemFake{reply: redeemReply(`"reset"`)}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	got := r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
	if !codexWeeklyExhausted(got) {
		t.Fatal("the still-exhausted refreshed reading must be published so the relay keeps holding")
	}
	st, ok := readRedeemState(t, r)
	if !ok || st.Status != codexRedeemRedeemed || st.LastResult != codexRedeemOutcomeReset {
		t.Fatalf("state = %+v, want redeemed", st)
	}
	for i := 0; i < 3; i++ {
		r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
	}
	if len(fake.calls()) != 1 {
		t.Fatalf("redemptions = %d, want exactly 1", len(fake.calls()))
	}
	// A healthy reading ends the episode.
	r.evaluate(context.Background(), redeemRecovered, redeemSeq(redeemRecovered))
	if _, ok := readRedeemState(t, r); ok {
		t.Fatal("a healthy reading must clear the attempt record")
	}
}

// A failed re-read after redemption is published as-is (unknown), so the
// relay keeps holding; it is never replaced by an assumed recovery.
func TestCodexResetRedeem_FailedRereadAfterRedeemPublishesUnknown(t *testing.T) {
	fake := &redeemFake{reply: redeemReply(`"reset"`)}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	failed := Headroom{Provider: "openai", ProbeErr: errors.New("probe failed")}
	got := r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted, failed))
	if got.ProbeErr == nil {
		t.Fatal("a failed re-read must not be replaced by a healthy reading")
	}
	if st, _ := readRedeemState(t, r); st.Status != codexRedeemRedeemed {
		t.Fatalf("state = %+v, want redeemed", st)
	}
}

func TestCodexResetRedeem_DefiniteRefusalBacksOffAndRetiresKey(t *testing.T) {
	for _, outcome := range []string{`"noCredit"`, `{"result":"nothingToReset"}`} {
		t.Run(outcome, func(t *testing.T) {
			clock := newRedeemClock()
			fake := &redeemFake{reply: redeemReply(outcome)}
			r := newTestRedeemer(t.TempDir(), fake, clock)
			got := r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
			if !codexWeeklyExhausted(got) {
				t.Fatal("a refusal holds")
			}
			st, _ := readRedeemState(t, r)
			if st.Status != codexRedeemBackoff || st.IdempotencyKey != "" || st.Failures != 1 {
				t.Fatalf("state = %+v, want backoff with the key retired", st)
			}
			// Within backoff: nothing is sent.
			r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
			if len(fake.calls()) != 1 {
				t.Fatal("backoff must suppress retries")
			}
			// After backoff a new attempt may use a new key: nothing was spent.
			clock.advance(codexResetRedeemBackoff(1) + time.Second)
			r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
			if calls := fake.calls(); len(calls) != 2 || calls[1] != "key-2" {
				t.Fatalf("calls = %v, want a second attempt with key-2", calls)
			}
		})
	}
}

func TestCodexResetRedeem_UnsupportedSchemaHoldsAndReusesKey(t *testing.T) {
	clock := newRedeemClock()
	fake := &redeemFake{reply: redeemReply(`{"weird":1}`)}
	r := newTestRedeemer(t.TempDir(), fake, clock)
	got := r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
	if !codexWeeklyExhausted(got) {
		t.Fatal("an unsupported reply holds")
	}
	st, _ := readRedeemState(t, r)
	if st.Status != codexRedeemPending || st.IdempotencyKey != "key-1" || st.LastResult != "unsupported_schema" {
		t.Fatalf("state = %+v, want pending key-1 unsupported_schema", st)
	}
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
	if len(fake.calls()) != 1 {
		t.Fatal("backoff must suppress the retry")
	}
	clock.advance(codexResetRedeemBackoff(1) + time.Second)
	fake.reply = redeemReply(`"alreadyRedeemed"`)
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted, redeemRecovered))
	if calls := fake.calls(); len(calls) != 2 || calls[1] != "key-1" {
		t.Fatalf("calls = %v, want the retry to reuse key-1", calls)
	}
}

func TestCodexResetRedeem_PlainErrorKeepsKey(t *testing.T) {
	fake := &redeemFake{reply: func(context.Context, string) (json.RawMessage, error) {
		return nil, errors.New("codex app-server error: {\"code\":-32601}")
	}}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
	st, _ := readRedeemState(t, r)
	if st.Status != codexRedeemPending || st.IdempotencyKey != "key-1" || st.LastResult != "error" {
		t.Fatalf("state = %+v, want pending key-1 error", st)
	}
}

// A timed-out request is uncertain: the key survives a restart and the next
// process retries with it instead of minting a new logical attempt.
func TestCodexResetRedeem_TimeoutThenRestartReusesKey(t *testing.T) {
	dir := t.TempDir()
	clock := newRedeemClock()
	hung := &redeemFake{reply: func(ctx context.Context, _ string) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}}
	r1 := newTestRedeemer(dir, hung, clock)
	r1.timeout = time.Millisecond
	r1.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
	st, _ := readRedeemState(t, r1)
	if st.Status != codexRedeemPending || st.LastResult != "timeout" || st.IdempotencyKey != "key-1" {
		t.Fatalf("state = %+v, want pending key-1 timeout", st)
	}

	clock.advance(codexResetRedeemBackoff(1) + time.Second)
	ok := &redeemFake{reply: redeemReply(`"alreadyRedeemed"`)}
	r2 := newTestRedeemer(dir, ok, clock)
	r2.newKey = func() string { t.Fatal("a restart must not mint a new key for an uncertain attempt"); return "" }
	got := r2.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted, redeemRecovered))
	if calls := ok.calls(); len(calls) != 1 || calls[0] != "key-1" {
		t.Fatalf("calls = %v, want the restarted retry to reuse key-1", calls)
	}
	if codexWeeklyExhausted(got) {
		t.Fatal("want the refreshed reading after alreadyRedeemed")
	}
}

func TestCodexResetRedeem_DeadlineErrorIsTimeout(t *testing.T) {
	fake := &redeemFake{reply: func(context.Context, string) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded
	}}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
	if st, _ := readRedeemState(t, r); st.LastResult != "timeout" {
		t.Fatalf("state = %+v, want timeout", st)
	}
}

// A crash after the key was persisted but before the request went out leaves
// a pending record with no backoff; the next run sends with that key.
func TestCodexResetRedeem_PersistedKeyBeforeSendIsReused(t *testing.T) {
	fake := &redeemFake{reply: redeemReply(`"reset"`)}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	var seen codexResetRedeemState
	fake.reply = func(context.Context, string) (json.RawMessage, error) {
		seen, _ = readRedeemState(t, r)
		return json.RawMessage(`"reset"`), nil
	}
	if err := r.saveState(codexResetRedeemState{Status: codexRedeemPending, IdempotencyKey: "persisted"}); err != nil {
		t.Fatal(err)
	}
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted, redeemRecovered))
	if calls := fake.calls(); len(calls) != 1 || calls[0] != "persisted" {
		t.Fatalf("calls = %v, want the persisted key", calls)
	}
	if seen.IdempotencyKey != "persisted" || seen.Status != codexRedeemPending {
		t.Fatalf("state at send time = %+v, want the key persisted before sending", seen)
	}
}

func TestCodexResetRedeem_KeyPersistedBeforeFirstSend(t *testing.T) {
	fake := &redeemFake{}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	var seen codexResetRedeemState
	fake.reply = func(context.Context, string) (json.RawMessage, error) {
		seen, _ = readRedeemState(t, r)
		return json.RawMessage(`"reset"`), nil
	}
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted, redeemRecovered))
	if seen.IdempotencyKey != "key-1" || seen.Status != codexRedeemPending {
		t.Fatalf("state at send time = %+v, want key-1 already persisted", seen)
	}
}

// The re-read under the lock is authoritative: if it no longer triggers,
// nothing is redeemed.
func TestCodexResetRedeem_RereadUnderLockDecides(t *testing.T) {
	t.Run("recovered meanwhile", func(t *testing.T) {
		fake := &redeemFake{reply: redeemReply(`"reset"`)}
		r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
		if err := r.saveState(codexResetRedeemState{Status: codexRedeemRedeemed}); err != nil {
			t.Fatal(err)
		}
		got := r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemRecovered))
		if len(fake.calls()) != 0 || codexWeeklyExhausted(got) {
			t.Fatal("a recovered re-read must be published and nothing redeemed")
		}
		if _, ok := readRedeemState(t, r); ok {
			t.Fatal("recovery clears the record")
		}
	})
	t.Run("credit gone", func(t *testing.T) {
		fake := &redeemFake{reply: redeemReply(`"reset"`)}
		r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
		r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemReading(0, 60, redeemIntp(0))))
		if len(fake.calls()) != 0 {
			t.Fatal("no credit on the re-read means no redemption")
		}
	})
	t.Run("re-read failed", func(t *testing.T) {
		fake := &redeemFake{reply: redeemReply(`"reset"`)}
		r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
		got := r.evaluate(context.Background(), redeemExhausted, redeemSeq(Headroom{Provider: "openai", ProbeErr: errors.New("x")}))
		if len(fake.calls()) != 0 || got.ProbeErr != nil {
			t.Fatal("a failed re-read is not a fresh reading: hold on the original")
		}
	})
}

func TestCodexResetRedeem_UnreadableStateHolds(t *testing.T) {
	fake := &redeemFake{reply: redeemReply(`"reset"`)}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	if err := os.WriteFile(r.statePath(), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted))
	if len(fake.calls()) != 0 {
		t.Fatal("an unreadable attempt record must never lead to a new attempt")
	}
}

func TestCodexResetRedeem_HeldLockHoldsAndStaleLockIsTakenOver(t *testing.T) {
	fake := &redeemFake{reply: redeemReply(`"reset"`)}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	if err := os.WriteFile(r.lockPath(), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted, redeemRecovered))
	if len(fake.calls()) != 0 {
		t.Fatal("a held lock means another controller owns the attempt")
	}
	old := time.Now().Add(-2 * codexResetRedeemLockStale)
	if err := os.Chtimes(r.lockPath(), old, old); err != nil {
		t.Fatal(err)
	}
	r.evaluate(context.Background(), redeemExhausted, redeemSeq(redeemExhausted, redeemRecovered))
	if len(fake.calls()) != 1 {
		t.Fatal("an abandoned lock must be taken over")
	}
	if _, err := os.Stat(r.lockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the lock must be released")
	}
}

// Two controllers for the same pool: while one is mid-request the other must
// not send; once it finishes, the other's re-read sees the recovery.
func TestCodexResetRedeem_ConcurrentAttemptsSpendOneReset(t *testing.T) {
	dir := t.TempDir()
	clock := newRedeemClock()
	var redeemed atomic.Bool
	provider := func(context.Context) Headroom {
		if redeemed.Load() {
			return redeemRecovered
		}
		return redeemExhausted
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	fake := &redeemFake{reply: func(context.Context, string) (json.RawMessage, error) {
		close(entered)
		<-release
		redeemed.Store(true)
		return json.RawMessage(`"reset"`), nil
	}}
	a := newTestRedeemer(dir, fake, clock)
	b := newTestRedeemer(dir, fake, clock)

	done := make(chan Headroom)
	go func() { done <- a.evaluate(context.Background(), redeemExhausted, provider) }()
	<-entered
	if got := b.evaluate(context.Background(), redeemExhausted, provider); !codexWeeklyExhausted(got) {
		t.Fatal("the losing controller holds on its reading")
	}
	close(release)
	if got := <-done; codexWeeklyExhausted(got) {
		t.Fatal("the winner publishes the refreshed reading")
	}
	b.evaluate(context.Background(), redeemExhausted, provider)
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("redemptions = %d, want exactly 1", n)
	}
}

func TestCodexResetRedeem_ManyConcurrentControllersSpendOneReset(t *testing.T) {
	dir := t.TempDir()
	clock := newRedeemClock()
	var redeemed atomic.Bool
	provider := func(context.Context) Headroom {
		if redeemed.Load() {
			return redeemRecovered
		}
		return redeemExhausted
	}
	fake := &redeemFake{reply: func(context.Context, string) (json.RawMessage, error) {
		redeemed.Store(true)
		return json.RawMessage(`"reset"`), nil
	}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			newTestRedeemer(dir, fake, clock).evaluate(context.Background(), redeemExhausted, provider)
		}()
	}
	close(start)
	wg.Wait()
	if n := len(fake.calls()); n != 1 {
		t.Fatalf("redemptions = %d, want exactly 1", n)
	}
}

func TestParseCodexRedeemOutcome(t *testing.T) {
	cases := map[string]string{
		`"reset"`:                          codexRedeemOutcomeReset,
		`{"status":"RESET"}`:               codexRedeemOutcomeReset,
		`{"outcome":"alreadyRedeemed"}`:    codexRedeemOutcomeAlreadyRedeemed,
		`{"result":" nothingToReset "}`:    codexRedeemOutcomeNothingToReset,
		`{"status":"noCredit"}`:            codexRedeemOutcomeNoCredit,
		`{"status":"granted"}`:             "",
		`{"status":7}`:                     "",
		`[]`:                               "",
		`not json`:                         "",
		`{"credits":{"availableCount":1}}`: "",
	}
	for raw, want := range cases {
		got, err := parseCodexRedeemOutcome(json.RawMessage(raw))
		if want == "" {
			if !errors.Is(err, errCodexRedeemUnsupportedSchema) {
				t.Errorf("%s: err = %v, want unsupported schema", raw, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestCodexResetRedeemBackoff(t *testing.T) {
	if got := codexResetRedeemBackoff(1); got != codexResetRedeemBackoffBase {
		t.Fatalf("first backoff = %v", got)
	}
	if got := codexResetRedeemBackoff(2); got != 2*codexResetRedeemBackoffBase {
		t.Fatalf("second backoff = %v", got)
	}
	if got := codexResetRedeemBackoff(50); got != codexResetRedeemBackoffMax {
		t.Fatalf("capped backoff = %v", got)
	}
}

func TestCodexResetRedeem_StateFilesLiveInPoolDir(t *testing.T) {
	dir := t.TempDir()
	r := newCodexResetRedeemer(dir, "codex", "acct")
	key := deriveContributorPoolKey("codex", "acct")
	if r.statePath() != filepath.Join(dir, key+codexResetRedeemStateSuffix) || r.lockPath() != filepath.Join(dir, key+codexResetRedeemLockSuffix) {
		t.Fatalf("paths %s %s must be pool-keyed in %s", r.statePath(), r.lockPath(), dir)
	}
	if codexResetCreditsLog(Headroom{}) != "unknown" || codexResetCreditsLog(redeemExhausted) != 2 {
		t.Fatal("credit log value")
	}
}
