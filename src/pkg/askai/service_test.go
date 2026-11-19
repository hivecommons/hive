package askai

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUpstream records every query and answers from a script.
type fakeUpstream struct {
	mu      sync.Mutex
	queries []string
	threads []string
	answer  UpstreamAnswer
	err     error
	block   chan struct{}
	// entered, when set, receives once per call that reached the vendor.
	entered chan struct{}
}

func (f *fakeUpstream) Chat(ctx context.Context, thread, query string) (UpstreamAnswer, error) {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.threads = append(f.threads, thread)
	block, answer, err, entered := f.block, f.answer, f.err, f.entered
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return UpstreamAnswer{}, ctx.Err()
		}
	}
	return answer, err
}

func (f *fakeUpstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeUpstream) set(a UpstreamAnswer, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer, f.err = a, err
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type allowAll struct{}

func (allowAll) Has(string) bool { return true }

var generous = Limits{PersonPerMinute: 100, PersonPerHour: 100, HivePerHour: 100, GlobalPerMinute: 100, InFlight: 10, Daily: 1000}

var citedAnswer = UpstreamAnswer{
	Answer:   "Open Settings, then Features.",
	ThreadID: "thread-1",
	Sources:  []UpstreamSource{{URL: "https://docs.hivecommons.dev/settings/#features", Title: "Settings"}},
}

func newTestService(t *testing.T, up *fakeUpstream, consent ConsentChecker, limits Limits, logs *bytes.Buffer, clock *fakeClock) *Service {
	t.Helper()
	if clock == nil {
		clock = &fakeClock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	}
	logger := slog.Default()
	if logs != nil {
		logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	svc, err := NewService(Options{
		Upstream: up, Limits: limits, SendSectionLabel: true, Consent: consent, Logger: logger, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestPayloadCarriesOnlyQuestionAndSectionLabel is the "what leaves" invariant:
// the vendor query is the question plus, at most, a label from our own closed
// list. Page data, the person, the hive and an unknown section id never appear.
func TestPayloadCarriesOnlyQuestionAndSectionLabel(t *testing.T) {
	up := &fakeUpstream{answer: citedAnswer}
	svc := newTestService(t, up, allowAll{}, generous, nil, nil)
	const q = "Where do I pause an agent?"
	seeded := []string{"alice-private-login", "acme-secret-hive", "acme/private-repo"}

	svc.Ask(context.Background(), Request{Person: seeded[0], HiveID: seeded[1], Question: q, Section: seeded[2]})
	svc.Ask(context.Background(), Request{Person: seeded[0], HiveID: seeded[1], Question: q, Section: "settings-features"})
	svc.opts.SendSectionLabel = false
	svc.Ask(context.Background(), Request{Person: seeded[0], HiveID: seeded[1], Question: q, Section: "settings-features"})

	want := []string{q, "Dashboard section: Settings, Features.\n\n" + q, q}
	if len(up.queries) != len(want) {
		t.Fatalf("queries %q", up.queries)
	}
	for i, got := range up.queries {
		if got != want[i] {
			t.Errorf("query %d = %q, want %q", i, got, want[i])
		}
		for _, s := range seeded {
			if strings.Contains(got, s) {
				t.Errorf("query %d leaks %q", i, s)
			}
		}
	}
}

func TestConsentGatesEveryRequest(t *testing.T) {
	store := &ConsentStore{Dir: t.TempDir()}
	up := &fakeUpstream{answer: citedAnswer}
	svc := newTestService(t, up, store, generous, nil, nil)
	ask := func(person string) Outcome {
		return svc.Ask(context.Background(), Request{Person: person, Question: "How do I add a repo?"}).Outcome
	}
	now := time.Now()
	if got := ask("alice"); got != OutcomeNotConsented || up.calls() != 0 {
		t.Fatalf("no consent: %s, calls %d", got, up.calls())
	}
	if _, err := store.Grant("alice", ConsentTextVersion, now); err != nil {
		t.Fatal(err)
	}
	if got := ask("alice"); got != OutcomeOK || up.calls() != 1 {
		t.Fatalf("consented: %s, calls %d", got, up.calls())
	}
	if got := ask("bob"); got != OutcomeNotConsented || up.calls() != 1 {
		t.Fatalf("alice's consent covered bob: %s", got)
	}
	if _, err := store.Withdraw("alice", now); err != nil {
		t.Fatal(err)
	}
	if got := ask("alice"); got != OutcomeNotConsented || up.calls() != 1 {
		t.Fatalf("withdrawn: %s", got)
	}
	// A record for an older disclosure text does not count.
	if err := writeConsent(store.Dir+"/carol.json", Consent{Person: "carol", TextVersion: "0", GrantedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got := ask("carol"); got != OutcomeNotConsented || up.calls() != 1 {
		t.Fatalf("stale text version: %s", got)
	}
	// No checker at all is "no consent", never "allow".
	open := newTestService(t, up, nil, generous, nil, nil)
	if got := open.Ask(context.Background(), Request{Person: "alice", Question: "q?"}).Outcome; got != OutcomeNotConsented {
		t.Fatalf("nil checker: %s", got)
	}
}

func TestCredentialShapedQuestionsAreRefusedAndNeverSent(t *testing.T) {
	up := &fakeUpstream{answer: citedAnswer}
	var logs bytes.Buffer
	svc := newTestService(t, up, allowAll{}, generous, &logs, nil)
	for _, q := range []string{
		"my token is ghp_abcdefghijklmnopqrstuvwxyz0123 why does it fail",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345",
		"the key sk-abcdef123456 is rejected",
		"AKIAABCDEFGHIJKLMNOP leaked?",
	} {
		r := svc.Ask(context.Background(), Request{Person: "alice", Question: q})
		if r.Outcome != OutcomeRefusedSecretShape || r.Message == "" || strings.Contains(r.Message, q) {
			t.Errorf("%q: %+v", q, r)
		}
	}
	if up.calls() != 0 {
		t.Fatalf("a credential-shaped question reached the vendor %d times", up.calls())
	}
	for _, frag := range []string{"ghp_", "sk-abcdef", "AKIA", "Bearer"} {
		if strings.Contains(logs.String(), frag) {
			t.Fatalf("audit log carries question text %q:\n%s", frag, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "outcome=refused_secret_shape") {
		t.Fatalf("no audit line:\n%s", logs.String())
	}
	if _, o := ValidateQuestion(strings.Repeat("a", MaxQuestionRunes+1)); o != OutcomeInvalidQuestion {
		t.Fatal("an over-long question must be invalid")
	}
	if _, o := ValidateQuestion("   "); o != OutcomeInvalidQuestion {
		t.Fatal("an empty question must be invalid")
	}
}

func TestAnswersNeedAllowlistedSourcesAndRenderAsText(t *testing.T) {
	up := &fakeUpstream{}
	svc := newTestService(t, up, allowAll{}, generous, nil, nil)
	ask := func() Result { return svc.Ask(context.Background(), Request{Person: "alice", Question: "where?"}) }

	up.set(UpstreamAnswer{Answer: "Somewhere.", Sources: []UpstreamSource{{URL: "https://evil.example/docs", Title: "x"}}}, nil)
	if r := ask(); r.Outcome != OutcomeNoCitedAnswer || r.Answer != "" || len(r.Sources) != 0 || !r.AIGenerated || r.DocsURL == "" {
		t.Fatalf("uncited answer shown as an answer: %+v", r)
	}
	up.set(UpstreamAnswer{
		Answer: "Click [here](https://evil.example/steal) or see https://evil.example/x and https://docs.hivecommons.dev/a/ <script>alert(1)</script>",
		Sources: []UpstreamSource{
			{URL: "https://docs.hivecommons.dev/a/", Title: "A\u0007"},
			{URL: "https://docs.hivecommons.dev/a/", Title: "dup"},
			{URL: "javascript:alert(1)", Title: "js"},
			{URL: "https://github.com/hivecommons/hive/blob/v5/src/docs/settings.md#features", Title: "Settings"},
			{URL: "https://docs.hivecommons.dev/internal/", Title: "Internal", Internal: true},
		},
	}, nil)
	r := ask()
	if r.Outcome != OutcomeOK || len(r.Sources) != 2 || r.Sources[0].Title != "A" ||
		r.Sources[1].URL != "https://github.com/hivecommons/hive/blob/v5/src/docs/settings.md#features" {
		t.Fatalf("sources: %+v", r)
	}
	if strings.Contains(r.Answer, "evil.example") || !strings.Contains(r.Answer, "Click here") ||
		!strings.Contains(r.Answer, "https://docs.hivecommons.dev/a/") {
		t.Fatalf("answer links not reduced: %q", r.Answer)
	}
	// Markup survives only as inert text for the page to show with textContent.
	if !strings.Contains(r.Answer, "<script>alert(1)</script>") {
		t.Fatalf("answer text altered unexpectedly: %q", r.Answer)
	}
}

func TestLimitsHoldPerPersonHiveAndGlobal(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	up := &fakeUpstream{answer: citedAnswer}
	limits := Limits{PersonPerMinute: 2, PersonPerHour: 3, HivePerHour: 4, GlobalPerMinute: 5, InFlight: 10, Daily: 8}
	var logs bytes.Buffer
	svc := newTestService(t, up, allowAll{}, limits, &logs, clock)
	ask := func(person, hive string) Result {
		return svc.Ask(context.Background(), Request{Person: person, HiveID: hive, Question: "q?"})
	}
	ask("a", "")
	ask("a", "")
	if r := ask("a", ""); r.Outcome != OutcomeRateLimited || r.RetryAfterSeconds <= 0 {
		t.Fatalf("per-person minute cap: %+v", r)
	}
	clock.advance(time.Minute)
	ask("a", "")
	clock.advance(time.Minute)
	if r := ask("a", ""); r.Outcome != OutcomeRateLimited {
		t.Fatalf("per-person hour cap: %+v", r)
	}
	// Hive cap: four relayed requests from different people of one hive.
	clock.advance(time.Minute)
	for _, p := range []string{"h1", "h2", "h1", "h2"} {
		if r := ask(p, "hive-a"); r.Outcome != OutcomeOK {
			t.Fatalf("hive request %s: %+v", p, r)
		}
		clock.advance(time.Minute)
	}
	if r := ask("h3", "hive-a"); r.Outcome != OutcomeRateLimited {
		t.Fatalf("per-hive cap: %+v", r)
	}
	if r := ask("h3", "hive-b"); r.Outcome != OutcomeOK {
		t.Fatalf("hive B must not be charged for hive A: %+v", r)
	}
	// That was the eighth call of the day: the daily budget is now spent for
	// everyone, and nothing more reaches the vendor.
	if r := ask("h4", "hive-c"); r.Outcome != OutcomeRateLimited {
		t.Fatalf("daily budget: %+v", r)
	}
	if got := up.calls(); got != limits.Daily {
		t.Fatalf("vendor calls %d, want exactly the daily budget %d", got, limits.Daily)
	}
	if !strings.Contains(logs.String(), "audit: ask daily budget spent") {
		t.Fatalf("daily budget line missing:\n%s", logs.String())
	}
	stats := svc.Stats()
	if stats.DailyBudget != limits.Daily || stats.ByOutcome[OutcomeRateLimited] == 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestGlobalPerMinuteAndInFlight(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	up := &fakeUpstream{answer: citedAnswer}
	svc := newTestService(t, up, allowAll{}, Limits{PersonPerMinute: 10, PersonPerHour: 10, HivePerHour: 10, GlobalPerMinute: 2, InFlight: 1, Daily: 100}, nil, clock)
	ask := func(p string) Outcome {
		return svc.Ask(context.Background(), Request{Person: p, Question: "q?"}).Outcome
	}
	if ask("a") != OutcomeOK || ask("b") != OutcomeOK || ask("c") != OutcomeRateLimited {
		t.Fatal("global per-minute cap did not hold")
	}
	clock.advance(time.Minute)

	up.mu.Lock()
	up.block = make(chan struct{})
	up.entered = make(chan struct{}, 1)
	up.mu.Unlock()
	done := make(chan Outcome)
	go func() { done <- ask("a") }()
	<-up.entered
	if got := ask("b"); got != OutcomeRateLimited {
		t.Fatalf("global in-flight cap: %s", got)
	}
	if got := ask("a"); got != OutcomeRateLimited {
		t.Fatalf("per-person in-flight cap: %s", got)
	}
	close(up.block)
	if got := <-done; got != OutcomeOK {
		t.Fatalf("blocked call: %s", got)
	}
}

func TestUpstreamFailuresFallBackAndTripTheBreaker(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	up := &fakeUpstream{}
	var logs bytes.Buffer
	svc := newTestService(t, up, allowAll{}, generous, &logs, clock)
	ask := func() Result { return svc.Ask(context.Background(), Request{Person: "a", Question: "q?"}) }

	// A vendor 429 is "busy", backs everyone off and is not a breaker failure.
	up.set(UpstreamAnswer{}, &UpstreamStatusError{Status: http.StatusTooManyRequests, RetryAfter: 10 * time.Second})
	if r := ask(); r.Outcome != OutcomeRateLimited || r.RetryAfterSeconds != 10 {
		t.Fatalf("429: %+v", r)
	}
	calls := up.calls()
	if r := ask(); r.Outcome != OutcomeRateLimited || up.calls() != calls {
		t.Fatalf("backoff not honoured: %+v", r)
	}
	clock.advance(11 * time.Second)

	up.set(UpstreamAnswer{}, context.DeadlineExceeded)
	for i := 0; i < DefaultBreakerFailures; i++ {
		if r := ask(); r.Outcome != OutcomeUpstreamError || r.DocsURL == "" {
			t.Fatalf("failure %d: %+v", i, r)
		}
	}
	calls = up.calls()
	if r := ask(); r.Outcome != OutcomeBreakerOpen || up.calls() != calls {
		t.Fatalf("open breaker still called out: %+v", r)
	}
	if !strings.Contains(logs.String(), "audit: ask breaker opened") {
		t.Fatal("no breaker-open audit line")
	}
	clock.advance(DefaultBreakerCooldown)
	up.set(citedAnswer, nil)
	if r := ask(); r.Outcome != OutcomeOK {
		t.Fatalf("trial after cool-down: %+v", r)
	}
	if !strings.Contains(logs.String(), "audit: ask breaker closed") || svc.Stats().BreakerOpen {
		t.Fatal("breaker did not close")
	}
	if strings.Contains(logs.String(), "q?") {
		t.Fatal("audit lines carry the question")
	}
}

func TestUpstreamTimeoutIsAFallback(t *testing.T) {
	up := &fakeUpstream{block: make(chan struct{})}
	defer close(up.block)
	svc, err := NewService(Options{Upstream: up, Limits: generous, Consent: allowAll{}, UpstreamTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if r := svc.Ask(context.Background(), Request{Person: "a", Question: "q?"}); r.Outcome != OutcomeUpstreamError {
		t.Fatalf("timeout: %+v", r)
	}
}

func TestConversationsAreBoundToPersonAndHive(t *testing.T) {
	up := &fakeUpstream{answer: citedAnswer}
	svc := newTestService(t, up, allowAll{}, generous, nil, nil)
	first := svc.Ask(context.Background(), Request{Person: "alice", HiveID: "hive-a", Question: "q?"})
	if first.Conversation == "" || strings.Contains(first.Conversation, citedAnswer.ThreadID) {
		t.Fatalf("handle %q must be opaque", first.Conversation)
	}
	svc.Ask(context.Background(), Request{Person: "alice", HiveID: "hive-a", Question: "and?", Conversation: first.Conversation})
	svc.Ask(context.Background(), Request{Person: "bob", HiveID: "hive-a", Question: "and?", Conversation: first.Conversation})
	svc.Ask(context.Background(), Request{Person: "alice", HiveID: "hive-b", Question: "and?", Conversation: first.Conversation})
	want := []string{"", citedAnswer.ThreadID, "", ""}
	for i, got := range up.threads {
		if got != want[i] {
			t.Fatalf("call %d used thread %q, want %q", i, got, want[i])
		}
	}
}

func TestNewServiceNeedsUpstream(t *testing.T) {
	if _, err := NewService(Options{}); err == nil {
		t.Fatal("a service without an upstream must be refused")
	}
}
