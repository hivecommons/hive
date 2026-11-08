package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is the injectable clock: tests move time, never sleep.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeTimers hands each After channel to the test, which fires it.
type fakeTimers struct{ ch chan chan time.Time }

func (f *fakeTimers) After(time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	f.ch <- c
	return c
}

var (
	operator = Caller{Name: "kelly", Principal: "operator:kelly", Operator: true}
	driver   = Caller{Name: "atlas", Principal: "project:atlas"}
)

type fixture struct {
	t      *testing.T
	dir    string
	clock  *fakeClock
	timers *fakeTimers
	svc    *Service
	n      int
	mu     sync.Mutex
	events []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: filepath.Join(t.TempDir(), DirName), clock: &fakeClock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)},
		timers: &fakeTimers{ch: make(chan chan time.Time, 8)}}
	f.restart()
	return f
}

// restart is a new process on the same directory; ids stay unique across
// restarts because the counter lives on the fixture.
func (f *fixture) restart() {
	f.t.Helper()
	svc, err := New(Config{Dir: f.dir, Now: f.clock.Now, After: f.timers.After, NewID: func() string {
		f.n++
		return fmt.Sprintf("%s%016x", IDPrefix, f.n)
	}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.events = nil
	f.mu.Unlock()
	svc.SetObserver(func(op string, rec Record) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = append(f.events, op+":"+rec.ID)
	})
	f.svc = svc
}

func (f *fixture) journal() []JournalEntry {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, JournalName))
	if err != nil {
		f.t.Fatal(err)
	}
	var out []JournalEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e JournalEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			f.t.Fatalf("journal line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func TestOpenIsIdempotentAndSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	req := Request{Subject: "Approve experiment budget for cache-17", Context: json.RawMessage(`{"b":2,"a":"x"}`), OpenedBy: "nous-campaign-42"}
	first, created, err := f.svc.Open(driver, req)
	if err != nil || !created {
		t.Fatalf("open: %v created=%v", err, created)
	}
	if first.Status != StatusWaiting || strings.Join(first.Choices, ",") != "approve,deny" || first.Timeout != "0" || first.ExpiresAt != nil ||
		first.OpenedBy != "nous-campaign-42" || first.OpenedByPrincipal != "project:atlas" || string(first.Context) != `{"a":"x","b":2}` {
		t.Fatalf("opened = %+v", first)
	}
	if _, err := os.Stat(filepath.Join(f.dir, PendingDirName, first.ID+".json")); err != nil {
		t.Fatalf("pending record: %v", err)
	}
	// The same request, context keys reordered, is the same gate.
	req.Context = json.RawMessage(`{"a":"x", "b":2}`)
	again, created, err := f.svc.Open(driver, req)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("reopen: %+v created=%v err=%v", again, created, err)
	}
	// Another principal asking the same thing gets its own gate.
	if other, created, err := f.svc.Open(Caller{Name: "beacon", Principal: "project:beacon"}, req); err != nil || !created || other.ID == first.ID {
		t.Fatalf("other principal: %+v created=%v err=%v", other, created, err)
	}

	f.restart()
	answered, err := f.svc.Answer(operator, first.ID, Answer{Choice: "approve", Note: "within budget"})
	if err != nil {
		t.Fatalf("answer after restart: %v", err)
	}
	if answered.Status != StatusAnswered || answered.Answer != "approve" || answered.AnsweredBy != "kelly" ||
		answered.AnsweredByPrincipal != "operator:kelly" || answered.Note != "within budget" || answered.OperatorOverride || answered.AnsweredAt == nil {
		t.Fatalf("answered = %+v", answered)
	}
	if _, err := os.Stat(filepath.Join(f.dir, PendingDirName, first.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending record left behind: %v", err)
	}

	f.restart()
	replay, created, err := f.svc.Open(driver, req)
	if err != nil || created || replay.ID != first.ID || replay.Answer != "approve" || replay.Status != StatusAnswered {
		t.Fatalf("reopen after answer and restart: %+v created=%v err=%v", replay, created, err)
	}
	ops := []string{}
	for _, e := range f.journal() {
		ops = append(ops, e.Op)
		if e.Op == OpAnswered && (e.By != "operator:kelly" || e.Answer != "approve") {
			t.Fatalf("answered entry = %+v", e)
		}
	}
	if strings.Join(ops, ",") != "opened,opened,answered" {
		t.Fatalf("journal ops = %v", ops)
	}
}

func TestCallerKeyIsScopedAndWins(t *testing.T) {
	f := newFixture(t)
	a, _, err := f.svc.Open(driver, Request{Subject: "first wording", IdempotencyKey: "step-7"})
	if err != nil {
		t.Fatal(err)
	}
	b, created, err := f.svc.Open(driver, Request{Subject: "second wording", IdempotencyKey: "step-7"})
	if err != nil || created || b.ID != a.ID || b.Subject != "first wording" {
		t.Fatalf("keyed reopen: %+v created=%v err=%v", b, created, err)
	}
}

func TestAnswerIsConstrained(t *testing.T) {
	f := newFixture(t)
	g, _, err := f.svc.Open(driver, Request{Subject: "ship it?", Choices: []string{"ship", "hold"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Answer(operator, g.ID, Answer{Choice: "approve"}); !errors.Is(err, ErrInvalidChoice) {
		t.Fatalf("off-menu answer: %v", err)
	}
	if _, err := f.svc.Answer(operator, g.ID, Answer{Choice: "yes, ship it"}); !errors.Is(err, ErrInvalidChoice) {
		t.Fatalf("free-text answer: %v", err)
	}
	if _, err := f.svc.Answer(driver, g.ID, Answer{Choice: "ship"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("project answer: %v", err)
	}
	if _, err := f.svc.Answer(operator, "gate_nope", Answer{Choice: "ship"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown gate: %v", err)
	}
	if _, err := f.svc.Answer(operator, "../../etc/passwd", Answer{Choice: "ship"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("path id: %v", err)
	}
	rec, err := f.svc.Answer(operator, g.ID, Answer{Choice: "hold", Note: "token ghp_secretvalue1234567890 leaked"})
	if err != nil || rec.Answer != "hold" || strings.Contains(rec.Note, "ghp_") {
		t.Fatalf("answer: %+v %v", rec, err)
	}
	// An exact replay is not an error and writes nothing; a different
	// answer to a closed gate is.
	if again, err := f.svc.Answer(operator, g.ID, Answer{Choice: "hold", Note: "token ghp_secretvalue1234567890 leaked"}); err != nil || again.AnsweredAt == nil || !again.AnsweredAt.Equal(*rec.AnsweredAt) {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err := f.svc.Answer(operator, g.ID, Answer{Choice: "ship"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("second answer: %v", err)
	}
	if n := len(f.journal()); n != 2 {
		t.Fatalf("journal has %d lines, want opened+answered", n)
	}
}

func TestOpenerNeedsExplicitOverride(t *testing.T) {
	f := newFixture(t)
	g, _, err := f.svc.Open(operator, Request{Subject: "self-serve"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Answer(operator, g.ID, Answer{Choice: "approve"}); !errors.Is(err, ErrSelfAnswer) {
		t.Fatalf("self answer: %v", err)
	}
	rec, err := f.svc.Answer(operator, g.ID, Answer{Choice: "approve", Override: true})
	if err != nil || !rec.OperatorOverride {
		t.Fatalf("override: %+v %v", rec, err)
	}
	j := f.journal()
	if last := j[len(j)-1]; !last.Override || last.By != "operator:kelly" {
		t.Fatalf("override not journaled: %+v", last)
	}
	// The override is recorded only when it was needed.
	other, _, _ := f.svc.Open(driver, Request{Subject: "theirs"})
	if rec, err := f.svc.Answer(operator, other.ID, Answer{Choice: "deny", Override: true}); err != nil || rec.OperatorOverride {
		t.Fatalf("unneeded override: %+v %v", rec, err)
	}
}

func TestExpiryIsNeitherApprovalNorDenial(t *testing.T) {
	f := newFixture(t)
	g, _, err := f.svc.Open(driver, Request{Subject: "quick decision", Timeout: time.Minute})
	if err != nil || g.ExpiresAt == nil || g.Timeout != "1m0s" {
		t.Fatalf("open: %+v %v", g, err)
	}
	forever, _, _ := f.svc.Open(driver, Request{Subject: "no rush"})
	f.clock.Advance(2 * time.Minute)
	f.restart()
	rec, err := f.svc.Get(g.ID)
	if err != nil || rec.Status != StatusExpired || rec.Answer != "" || rec.AnsweredBy != "" || rec.ExpiredAt == nil {
		t.Fatalf("expired = %+v %v", rec, err)
	}
	if _, err := f.svc.Answer(operator, g.ID, Answer{Choice: "approve"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("answer after expiry: %v", err)
	}
	f.clock.Advance(10000 * time.Hour)
	if rec, _ := f.svc.Get(forever.ID); rec.Status != StatusWaiting {
		t.Fatalf("timeout 0 expired: %+v", rec)
	}
	waiting, _ := f.svc.List(ListFilter{Status: StatusWaiting})
	if len(waiting) != 1 || waiting[0].ID != forever.ID {
		t.Fatalf("waiting = %+v", waiting)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.events, ",") != "expired:"+g.ID {
		t.Fatalf("events = %v", f.events)
	}
}

func TestRequestValidation(t *testing.T) {
	f := newFixture(t)
	big := `{"blob":"` + strings.Repeat("x", MaxContextBytes) + `"}`
	for _, tc := range []struct {
		req  Request
		want error
	}{
		{Request{}, ErrBadRequest},
		{Request{Subject: "s", Context: json.RawMessage(`{"a":`)}, ErrBadRequest},
		{Request{Subject: "s", Context: json.RawMessage(`{} {}`)}, ErrBadRequest},
		{Request{Subject: "s", Choices: []string{"a", "a"}}, ErrBadRequest},
		{Request{Subject: "s", Choices: []string{"a", " "}}, ErrBadRequest},
		{Request{Subject: "s", Timeout: -time.Second}, ErrBadRequest},
		{Request{Subject: "s", Context: json.RawMessage(big)}, ErrContextTooLarge},
	} {
		if _, _, err := f.svc.Open(driver, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("Open(%+v) = %v, want %v", tc.req.Choices, err, tc.want)
		}
	}
	rec, _, err := f.svc.Open(driver, Request{Subject: "s", Context: json.RawMessage(`{"cred":"ghp_secretvalue1234567890","html":"<b>"}`)})
	if err != nil || strings.Contains(string(rec.Context), "ghp_") || !strings.Contains(string(rec.Context), `"<b>"`) {
		t.Fatalf("scrubbed context = %s %v", rec.Context, err)
	}
	for _, s := range []string{"", "0", " 0 "} {
		if d, err := ParseTimeout(s); err != nil || d != 0 {
			t.Fatalf("ParseTimeout(%q) = %v %v", s, d, err)
		}
	}
	if _, err := ParseTimeout("soon"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("ParseTimeout(soon) = %v", err)
	}
}

func TestWaitWakesOnAnswerExpiryAndWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	g, _, _ := f.svc.Open(driver, Request{Subject: "answer me"})
	done := make(chan Record, 1)
	go func() {
		rec, _ := f.svc.Wait(ctx, g.ID, 0)
		done <- rec
	}()
	if _, err := f.svc.Answer(operator, g.ID, Answer{Choice: "deny"}); err != nil {
		t.Fatal(err)
	}
	if rec := <-done; rec.Status != StatusAnswered || rec.Answer != "deny" {
		t.Fatalf("wait after answer = %+v", rec)
	}

	timed, _, _ := f.svc.Open(driver, Request{Subject: "expire me", Timeout: time.Minute})
	go func() {
		rec, _ := f.svc.Wait(ctx, timed.ID, 0)
		done <- rec
	}()
	expiry := <-f.timers.ch
	f.clock.Advance(time.Minute)
	expiry <- f.clock.Now()
	if rec := <-done; rec.Status != StatusExpired {
		t.Fatalf("wait through expiry = %+v", rec)
	}

	open, _, _ := f.svc.Open(driver, Request{Subject: "nobody home"})
	go func() {
		rec, _ := f.svc.Wait(ctx, open.ID, 30*time.Second)
		done <- rec
	}()
	window := <-f.timers.ch
	window <- f.clock.Now()
	if rec := <-done; rec.Status != StatusWaiting || rec.ID != open.ID {
		t.Fatalf("wait window = %+v", rec)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.svc.Wait(cctx, open.ID, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
}
