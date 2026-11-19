package ask

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/config"
)

type fakeChat struct {
	mu      sync.Mutex
	queries []string
	threads []string
	resp    ChatResponse
	err     error
	block   chan struct{}
}

func (f *fakeChat) Chat(ctx context.Context, query, threadID string) (ChatResponse, error) {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.threads = append(f.threads, threadID)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return f.resp, f.err
}

func (f *fakeChat) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func goodResponse() ChatResponse {
	return ChatResponse{
		Answer: "Open Settings, then Features.", ThreadID: "vendor-thread-1", IsUncertain: false,
		RelevantSources: []VendorSource{{SourceURL: "https://docs.hivecommons.dev/docs/dashboard", Title: "Dashboard"}},
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)} }

func testSettings() config.AskSettings {
	return config.AskSettings{
		Active: true, Mode: config.AskModeDirect, SendSectionLabel: true,
		Limits: config.AskLimits{
			PersonBurst: 3, PersonPerHour: 30, HivePerHour: 120, GlobalPerMinute: 8,
			InFlight: 4, Daily: 500, QuestionChars: 200, AnswerChars: 60, TimeoutSeconds: 5,
		},
	}
}

func newService(t *testing.T, st config.AskSettings, f *fakeChat, c *clock) (*Service, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var now func() time.Time
	if c != nil {
		now = c.now
	}
	svc, err := Open(st, logger, Options{Chatter: f, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return svc, &logs
}

func TestAskReturnsCitedAnswer(t *testing.T) {
	f := &fakeChat{resp: goodResponse()}
	svc, _ := newService(t, testSettings(), f, nil)
	res := svc.Ask(context.Background(), Request{Person: "alice", Question: "where is Features?"})
	if res.Outcome != OutcomeOK || res.Answer == "" || len(res.Sources) != 1 || res.Thread == "" || res.Message != "" {
		t.Fatalf("result = %+v", res)
	}
	if res.Thread == "vendor-thread-1" {
		t.Fatal("the vendor thread id must not reach the browser")
	}
}

func TestOnlyTheQuestionAndSectionLabelAreSent(t *testing.T) {
	f := &fakeChat{resp: goodResponse()}
	svc, _ := newService(t, testSettings(), f, nil)
	// Seeded identifiers that exist in the process but must never be sent.
	person, hive := "secret-login-bob", "secret-hive-name"
	svc.Ask(context.Background(), Request{
		Person: person, Hive: hive, Question: "where is the toggle?", Section: "settings-features",
	})
	want := "Dashboard section: Settings, Features\n\nwhere is the toggle?"
	if len(f.queries) != 1 || f.queries[0] != want {
		t.Fatalf("query = %q, want %q", f.queries, want)
	}
	for _, seeded := range []string{person, hive} {
		if strings.Contains(f.queries[0], seeded) {
			t.Fatalf("query carries %q", seeded)
		}
	}
}

func TestSectionLabelDroppedWhenUnknownOrSwitchedOff(t *testing.T) {
	f := &fakeChat{resp: goodResponse()}
	svc, _ := newService(t, testSettings(), f, nil)
	svc.Ask(context.Background(), Request{Person: "a", Question: "q1", Section: "not a section; repo=private/repo"})
	st := testSettings()
	st.SendSectionLabel = false
	svc2, _ := newService(t, st, f, nil)
	svc2.Ask(context.Background(), Request{Person: "a", Question: "q2", Section: "settings-features"})
	if f.queries[0] != "q1" || f.queries[1] != "q2" {
		t.Fatalf("queries = %q", f.queries)
	}
}

func TestCredentialShapedInputIsRefusedAndNeverSent(t *testing.T) {
	f := &fakeChat{resp: goodResponse()}
	svc, logs := newService(t, testSettings(), f, nil)
	token := "ghp_" + strings.Repeat("a1B2c3D4e5", 4)
	res := svc.Ask(context.Background(), Request{Person: "alice", Question: "why does " + token + " fail?"})
	if res.Outcome != OutcomeRefusedSecret || res.Message == "" || res.Answer != "" {
		t.Fatalf("result = %+v", res)
	}
	if f.calls() != 0 {
		t.Fatal("a credential-shaped question reached the vendor")
	}
	if strings.Contains(logs.String(), token) || strings.Contains(res.Message, token) {
		t.Fatal("the credential was echoed")
	}
}

func TestLengthAndEmptyCaps(t *testing.T) {
	f := &fakeChat{resp: goodResponse()}
	svc, _ := newService(t, testSettings(), f, nil)
	if r := svc.Ask(context.Background(), Request{Person: "a", Question: "   "}); r.Outcome != OutcomeRefusedEmpty {
		t.Fatalf("empty: %+v", r)
	}
	if r := svc.Ask(context.Background(), Request{Person: "a", Question: strings.Repeat("é", 201)}); r.Outcome != OutcomeRefusedLength {
		t.Fatalf("long: %+v", r)
	}
	if r := svc.Ask(context.Background(), Request{Person: "a", Question: strings.Repeat("é", 200)}); r.Outcome != OutcomeOK {
		t.Fatalf("at the cap: %+v", r)
	}
	if f.calls() != 1 {
		t.Fatalf("vendor calls = %d", f.calls())
	}
}

func TestNoAllowlistedSourceIsNoCitedAnswer(t *testing.T) {
	f := &fakeChat{resp: ChatResponse{
		Answer: "Click <a href=\"https://evil.example\">here</a>", ThreadID: "t",
		RelevantSources: []VendorSource{{SourceURL: "https://evil.example/x", Title: "x"}},
	}}
	svc, _ := newService(t, testSettings(), f, nil)
	res := svc.Ask(context.Background(), Request{Person: "a", Question: "q"})
	if res.Outcome != OutcomeNoCitedAnswer || res.Answer != "" || len(res.Sources) != 0 || res.Thread != "" || res.Message == "" {
		t.Fatalf("uncited text must not be shown as an answer: %+v", res)
	}
}

func TestAnswerIsPlainCappedText(t *testing.T) {
	r := goodResponse()
	r.Answer = "<script>alert(1)</script>\x1b[31m" + strings.Repeat("x", 500)
	svc, _ := newService(t, testSettings(), &fakeChat{resp: r}, nil)
	res := svc.Ask(context.Background(), Request{Person: "a", Question: "q"})
	if res.Outcome != OutcomeOK || len([]rune(res.Answer)) > 60 || strings.ContainsRune(res.Answer, '\x1b') {
		t.Fatalf("answer = %q", res.Answer)
	}
}

func TestThreadHandleIsBoundToThePerson(t *testing.T) {
	f := &fakeChat{resp: goodResponse()}
	svc, _ := newService(t, testSettings(), f, nil)
	first := svc.Ask(context.Background(), Request{Person: "alice", Question: "one"})
	// Alice continues her thread.
	second := svc.Ask(context.Background(), Request{Person: "alice", Question: "two", Thread: first.Thread})
	// Bob presents Alice's handle: it must start a new conversation.
	third := svc.Ask(context.Background(), Request{Person: "bob", Question: "three", Thread: first.Thread})
	if f.threads[0] != "" || f.threads[1] != "vendor-thread-1" || f.threads[2] != "" {
		t.Fatalf("vendor threads used = %q", f.threads)
	}
	if second.Thread != first.Thread {
		t.Fatalf("a continued conversation keeps its handle: %q vs %q", second.Thread, first.Thread)
	}
	if third.Thread == first.Thread || third.Thread == "" {
		t.Fatalf("bob must get his own handle, got %q", third.Thread)
	}
	// Withdrawal drops the conversation.
	svc.Forget("alice")
	svc.Ask(context.Background(), Request{Person: "alice", Question: "four", Thread: first.Thread})
	if f.threads[3] != "" {
		t.Fatalf("thread survived Forget: %q", f.threads)
	}
}

func TestThreadHandleExpires(t *testing.T) {
	clk := newClock()
	f := &fakeChat{resp: goodResponse()}
	svc, _ := newService(t, testSettings(), f, clk)
	first := svc.Ask(context.Background(), Request{Person: "alice", Question: "one"})
	clk.advance(DefaultThreadTTL + time.Minute)
	svc.Ask(context.Background(), Request{Person: "alice", Question: "two", Thread: first.Thread})
	if f.threads[1] != "" {
		t.Fatalf("expired thread was used: %q", f.threads)
	}
}

func TestPersonLimits(t *testing.T) {
	clk := newClock()
	f := &fakeChat{resp: goodResponse()}
	svc, _ := newService(t, testSettings(), f, clk)
	ask := func(p string) Result {
		return svc.Ask(context.Background(), Request{Person: p, Question: "q"})
	}
	for i := 0; i < 3; i++ {
		if r := ask("alice"); r.Outcome != OutcomeOK {
			t.Fatalf("request %d: %+v", i, r)
		}
	}
	r := ask("alice")
	if r.Outcome != OutcomeRateLimited || r.RetryAfterSeconds < 1 || r.RetryAfterSeconds > 60 {
		t.Fatalf("burst should be limited with a retry hint: %+v", r)
	}
	if ask("bob").Outcome != OutcomeOK {
		t.Fatal("one person's burst limited another")
	}
	clk.advance(61 * time.Second)
	if ask("alice").Outcome != OutcomeOK {
		t.Fatal("burst window did not reopen")
	}
	// Hourly cap: 30 per hour, with the burst respected by spacing requests.
	hourly := testSettings()
	hourly.Limits.PersonPerHour = 4
	svc2, _ := newService(t, hourly, f, clk)
	for i := 0; i < 4; i++ {
		if r := svc2.Ask(context.Background(), Request{Person: "carol", Question: "q"}); r.Outcome != OutcomeOK {
			t.Fatalf("hourly %d: %+v", i, r)
		}
		clk.advance(61 * time.Second)
	}
	if r := svc2.Ask(context.Background(), Request{Person: "carol", Question: "q"}); r.Outcome != OutcomeRateLimited {
		t.Fatalf("hourly cap did not hold: %+v", r)
	}
}

func TestHiveGlobalAndDailyLimits(t *testing.T) {
	clk := newClock()
	f := &fakeChat{resp: goodResponse()}
	st := testSettings()
	st.Limits.HivePerHour = 2
	st.Limits.PersonBurst = 100
	st.Limits.GlobalPerMinute = 5
	svc, _ := newService(t, st, f, clk)
	ask := func(person, hive string) Result {
		return svc.Ask(context.Background(), Request{Person: person, Hive: hive, Question: "q"})
	}
	if ask("a", "hive-1").Outcome != OutcomeOK || ask("b", "hive-1").Outcome != OutcomeOK {
		t.Fatal("hive allowance")
	}
	if r := ask("c", "hive-1"); r.Outcome != OutcomeRateLimited {
		t.Fatalf("one hive exceeded its allowance: %+v", r)
	}
	if ask("c", "hive-2").Outcome != OutcomeOK {
		t.Fatal("one hive's limit spent another hive's allowance")
	}
	// Global: 3 used so far, 2 more allowed this minute.
	if ask("d", "hive-3").Outcome != OutcomeOK || ask("e", "hive-4").Outcome != OutcomeOK {
		t.Fatal("global allowance")
	}
	if r := ask("f", "hive-5"); r.Outcome != OutcomeRateLimited {
		t.Fatalf("hub-wide per-minute cap did not hold: %+v", r)
	}

	// Daily budget.
	daily := testSettings()
	daily.Limits.Daily = 2
	daily.Limits.PersonBurst = 100
	daily.Limits.GlobalPerMinute = 100
	svc2, _ := newService(t, daily, f, clk)
	for i := 0; i < 2; i++ {
		svc2.Ask(context.Background(), Request{Person: "a", Question: "q"})
	}
	r := svc2.Ask(context.Background(), Request{Person: "a", Question: "q"})
	if r.Outcome != OutcomeBudgetSpent || r.RetryAfterSeconds < 1 {
		t.Fatalf("daily budget: %+v", r)
	}
	clk.advance(24 * time.Hour)
	if r := svc2.Ask(context.Background(), Request{Person: "a", Question: "q"}); r.Outcome != OutcomeOK {
		t.Fatalf("budget did not reset: %+v", r)
	}
	if _, today := svc2.Counters(); today != 1 {
		t.Fatalf("sent today = %d", today)
	}
}

func TestInFlightLimits(t *testing.T) {
	f := &fakeChat{resp: goodResponse(), block: make(chan struct{})}
	st := testSettings()
	st.Limits.InFlight = 2
	svc, _ := newService(t, st, f, nil)
	var wg sync.WaitGroup
	start := func(person string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.Ask(context.Background(), Request{Person: person, Question: "q"})
		}()
	}
	start("a")
	start("b")
	deadline := time.Now().Add(2 * time.Second)
	for f.calls() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r := svc.Ask(context.Background(), Request{Person: "c", Question: "q"}); r.Outcome != OutcomeRateLimited {
		t.Fatalf("hub-wide in-flight cap did not hold: %+v", r)
	}
	if r := svc.Ask(context.Background(), Request{Person: "a", Question: "again"}); r.Outcome != OutcomeRateLimited {
		t.Fatalf("a second request from the same person while one is in flight: %+v", r)
	}
	close(f.block)
	wg.Wait()
	if r := svc.Ask(context.Background(), Request{Person: "c", Question: "q"}); r.Outcome != OutcomeOK {
		t.Fatalf("slots were not released: %+v", r)
	}
}

func TestVendor429AndTimeoutGiveFallback(t *testing.T) {
	f := &fakeChat{err: ErrRateLimited}
	svc, _ := newService(t, testSettings(), f, nil)
	r := svc.Ask(context.Background(), Request{Person: "a", Question: "q"})
	if r.Outcome != OutcomeRateLimited || r.RetryAfterSeconds < 1 || r.Message == "" || r.Answer != "" {
		t.Fatalf("429: %+v", r)
	}
	f.err = ErrUnavailable
	r = svc.Ask(context.Background(), Request{Person: "b", Question: "q"})
	if r.Outcome != OutcomeUpstreamError || r.Message == "" {
		t.Fatalf("unavailable: %+v", r)
	}
}

func TestCircuitBreakerOpensAndCloses(t *testing.T) {
	clk := newClock()
	f := &fakeChat{err: ErrUnavailable}
	st := testSettings()
	st.Limits.PersonBurst = 1000
	st.Limits.PersonPerHour = 1000
	st.Limits.GlobalPerMinute = 1000
	svc, logs := newService(t, st, f, clk)
	ask := func() Result { return svc.Ask(context.Background(), Request{Person: "a", Question: "q"}) }
	for i := 0; i < breakerThreshold; i++ {
		if r := ask(); r.Outcome != OutcomeUpstreamError {
			t.Fatalf("failure %d: %+v", i, r)
		}
	}
	before := f.calls()
	if r := ask(); r.Outcome != OutcomeBreakerOpen {
		t.Fatalf("breaker should be open: %+v", r)
	}
	if f.calls() != before {
		t.Fatal("an open breaker still called the vendor")
	}
	if !strings.Contains(logs.String(), "circuit breaker opened") {
		t.Fatal("no audit line when the breaker opened")
	}
	// After the cool-down one probe is let through; a failed probe re-opens.
	clk.advance(breakerCooldown + time.Second)
	if r := ask(); r.Outcome != OutcomeUpstreamError {
		t.Fatalf("probe: %+v", r)
	}
	if r := ask(); r.Outcome != OutcomeBreakerOpen {
		t.Fatalf("a failed probe must re-open: %+v", r)
	}
	// A successful probe closes it.
	clk.advance(breakerCooldown + time.Second)
	f.err, f.resp = nil, goodResponse()
	if r := ask(); r.Outcome != OutcomeOK {
		t.Fatalf("good probe: %+v", r)
	}
	if r := ask(); r.Outcome != OutcomeOK {
		t.Fatalf("closed breaker: %+v", r)
	}
	if !strings.Contains(logs.String(), "circuit breaker closed") {
		t.Fatal("no audit line when the breaker closed")
	}
}

func TestAuditLinesAndCountersCarryNoText(t *testing.T) {
	f := &fakeChat{resp: goodResponse()}
	svc, logs := newService(t, testSettings(), f, nil)
	question := "unique-question-text-zzz"
	svc.Ask(context.Background(), Request{Person: "alice", Hive: "hive-1", Question: question})
	svc.Ask(context.Background(), Request{Person: "alice", Question: "ghp_" + strings.Repeat("a1B2c3D4e5", 4)})
	svc.RecordNotConsented("alice", "", 12)
	out := logs.String()
	for _, banned := range []string{question, "Open Settings", "docs.hivecommons.dev", "vendor-thread-1", "ghp_"} {
		if strings.Contains(out, banned) {
			t.Fatalf("audit output carries %q:\n%s", banned, out)
		}
	}
	for _, want := range []string{"outcome=ok", "outcome=refused_secret_shape", "outcome=not_consented", "question_len=", "sources=1", "person=alice", "hive=hive-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}
	counts, _ := svc.Counters()
	if counts[OutcomeOK] != 1 || counts[OutcomeRefusedSecret] != 1 || counts[OutcomeNotConsented] != 1 {
		t.Fatalf("counters = %v", counts)
	}
}

func TestRateLimitAuditIsOncePerWindow(t *testing.T) {
	clk := newClock()
	st := testSettings()
	st.Limits.PersonBurst = 1
	svc, logs := newService(t, st, &fakeChat{resp: goodResponse()}, clk)
	for i := 0; i < 6; i++ {
		svc.Ask(context.Background(), Request{Person: "alice", Question: "q"})
	}
	if n := strings.Count(logs.String(), "outcome=rate_limited"); n != 1 {
		t.Fatalf("rate limit audit lines = %d, want 1", n)
	}
	if counts, _ := svc.Counters(); counts[OutcomeRateLimited] != 5 {
		t.Fatalf("every limited request still counts: %v", counts)
	}
}

func TestOpenStaysOffWithoutAUsableKey(t *testing.T) {
	st := testSettings()
	if _, err := Open(config.AskSettings{}, nil, Options{}); !errors.Is(err, ErrNotActive) {
		t.Fatalf("inactive settings: %v", err)
	}
	dir := t.TempDir()
	defer cruxconfig.SetSecretFileRootsForTest(dir)()
	st.ProjectID, st.IntegrationID = testProjectID, testIntegrationID

	st.APIKeyFile = filepath.Join(dir, "missing")
	if _, err := Open(st, nil, Options{}); !errors.Is(err, ErrKeyUnreadable) || strings.Contains(err.Error(), dir) {
		t.Fatalf("missing key file: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.APIKeyFile = empty
	if _, err := Open(st, nil, Options{}); !errors.Is(err, ErrKeyUnreadable) {
		t.Fatalf("empty key file: %v", err)
	}
	st.APIKeyFile = filepath.Join(t.TempDir(), "outside") // not under the allowed root
	if err := os.WriteFile(st.APIKeyFile, []byte(testAPIKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(st, nil, Options{}); !errors.Is(err, ErrKeyUnreadable) {
		t.Fatalf("key outside the allowed directory must not be read: %v", err)
	}
}

// TestEndToEndAgainstFakeVendorHidesSeededValues drives the real client against
// a fake vendor with seeded secrets, repo names, hive names and logins in the
// surrounding process, and checks none reaches the wire, a response or a log.
func TestEndToEndAgainstFakeVendorHidesSeededValues(t *testing.T) {
	dir := t.TempDir()
	defer cruxconfig.SetSecretFileRootsForTest(dir)()
	keyFile := filepath.Join(dir, "kapa-key")
	if err := os.WriteFile(keyFile, []byte(testAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []recorded
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		seen = append(seen, recorded{r.URL.Path, r.Method, r.Header.Get("X-API-KEY"), "", b.Bytes()})
		_, _ = w.Write([]byte(contractResponse))
	}))
	defer vendor.Close()

	st := testSettings()
	st.ProjectID, st.IntegrationID, st.APIKeyFile = testProjectID, testIntegrationID, keyFile
	var logs bytes.Buffer
	svc, err := Open(st, slog.New(slog.NewTextHandler(&logs, nil)), Options{BaseURL: vendor.URL})
	if err != nil {
		t.Fatal(err)
	}
	seeded := []string{"acme/private-repo", "hive-prod-eu", "login-of-bob", "ghs_" + strings.Repeat("Z9y8X7w6V5", 3)}
	res := svc.Ask(context.Background(), Request{
		Person: "login-of-bob", Hive: "hive-prod-eu", Question: "how do I add a repo?", Section: "settings-repos",
	})
	if res.Outcome != OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	if len(seen) != 1 || seen[0].key != testAPIKey {
		t.Fatalf("vendor saw %+v", seen)
	}
	wire := string(seen[0].body)
	if !strings.Contains(wire, "how do I add a repo?") || !strings.Contains(wire, "Settings, Repositories") {
		t.Fatalf("wire body missing the question or label: %s", wire)
	}
	for _, v := range seeded {
		if strings.Contains(wire, v) {
			t.Fatalf("wire body carries %q: %s", v, wire)
		}
	}
	// Nothing credential-like in a response or the logs.
	out := logs.String() + res.Message + res.Answer + res.Thread
	for _, v := range []string{testAPIKey, testProjectID, testIntegrationID, vendor.URL} {
		if strings.Contains(out, v) {
			t.Fatalf("output carries %q", v)
		}
	}
}
