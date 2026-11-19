package live

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/meetings"
)

// fakeMeetings is an in-memory Meetings.
type fakeMeetings struct {
	mu      sync.Mutex
	m       map[string]meetings.Meeting
	posts   []string
	says    []string
	postErr error
	sayErr  error
	subs    []func(meetings.Update)
	// sayRes is what a say call reports; sayHook, when set, runs inside
	// each say call (a slow or blocked platform).
	sayRes  meetings.SayResult
	sayHook func(text string)
	// left records Leave calls (the reason), for a fake that can leave.
	left []string
}

func (f *fakeMeetings) Get(id string) (meetings.Meeting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[id]
	if !ok {
		return meetings.Meeting{}, meetings.ErrNotFound
	}
	return m, nil
}

func (f *fakeMeetings) PostChat(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return f.postErr
	}
	f.posts = append(f.posts, text)
	return nil
}

func (f *fakeMeetings) Say(_ context.Context, _ string, text string) (meetings.SayResult, error) {
	f.mu.Lock()
	hook := f.sayHook
	f.mu.Unlock()
	if hook != nil {
		hook(text)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sayErr != nil {
		return meetings.SayResult{}, f.sayErr
	}
	f.says = append(f.says, text)
	return f.sayRes, nil
}

// Leave implements Leaver.
func (f *fakeMeetings) Leave(_ context.Context, id, reason string) (meetings.Meeting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.left = append(f.left, reason)
	return f.m[id], nil
}

func (f *fakeMeetings) leaves() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.left...)
}

// fail sets the errors the next post and say calls return.
func (f *fakeMeetings) fail(post, say error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postErr, f.sayErr = post, say
}

func (f *fakeMeetings) Subscribe(fn func(meetings.Update)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, fn)
	return func() {}
}

func (f *fakeMeetings) push(u meetings.Update) {
	f.mu.Lock()
	subs := append([]func(meetings.Update){}, f.subs...)
	f.m[u.Meeting.ID] = u.Meeting
	f.mu.Unlock()
	for _, fn := range subs {
		fn(u)
	}
}

func (f *fakeMeetings) got() (posts, says []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...), append([]string(nil), f.says...)
}

// fakeSpek records stdin and lets the test write intents.
type fakeSpek struct {
	mu       sync.Mutex
	in       []string
	lines    chan []byte
	startErr error
	sendErr  error
	closed   bool
	once     sync.Once
}

func newFakeSpek() *fakeSpek { return &fakeSpek{lines: make(chan []byte, 64)} }

func (f *fakeSpek) Start(context.Context) error { return f.startErr }

func (f *fakeSpek) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.SendLine(b)
}

func (f *fakeSpek) SendLine(line []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("closed")
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	f.in = append(f.in, string(line))
	return nil
}

func (f *fakeSpek) Lines() <-chan []byte { return f.lines }

func (f *fakeSpek) Close(last any) {
	if last != nil {
		_ = f.Send(last)
	}
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.once.Do(func() { close(f.lines) })
}

func (f *fakeSpek) intent(s string) { f.lines <- []byte(s) }

func (f *fakeSpek) stdin() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.in...)
}

func (f *fakeSpek) has(sub string) bool {
	for _, l := range f.stdin() {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

type fakeResearch struct {
	mu    sync.Mutex
	n     int
	err   error
	metas []map[string]string
	// busy is what Busy reports (ResearchLoad).
	busy bool
}

// Busy implements ResearchLoad.
func (f *fakeResearch) Busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}

func (f *fakeResearch) setBusy(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = b
}

func (f *fakeResearch) Dispatch(_ context.Context, q string, meta map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.n++
	f.metas = append(f.metas, meta)
	return "kick-" + meta["rid"], nil
}

type fakeNotes struct {
	mu    sync.Mutex
	facts map[string]string
	err   error
}

func (f *fakeNotes) Upsert(_ context.Context, title, body string, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.facts == nil {
		f.facts = map[string]string{}
	}
	f.facts[title] = body
	return nil
}

func (f *fakeNotes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.facts)
}

type harness struct {
	t     *testing.T
	mt    *fakeMeetings
	spek  *fakeSpek
	res   *fakeResearch
	notes *fakeNotes
	r     *Runner
	store *meetings.Store
	m     meetings.Meeting
	// drops records the persona's skipped candidates (persona tests).
	drops *dropLog
}

func newHarness(t *testing.T, p Policy, mutate func(*Options)) *harness {
	t.Helper()
	st, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, mt: &fakeMeetings{m: map[string]meetings.Meeting{}}, spek: newFakeSpek(), res: &fakeResearch{}, notes: &fakeNotes{}, store: st}
	h.m = meetings.Meeting{ID: "mtg-1", Platform: meetings.PlatformGoogleMeet, State: meetings.StateLive, CreatedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	if err := st.Save(h.m); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Meetings: h.mt, Store: st, NewSpek: func(meetings.Meeting) Spek { return h.spek },
		Research: h.res, Notes: h.notes, Policy: p,
	}
	if mutate != nil {
		mutate(&opts)
	}
	r, err := NewRunner(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.r = r
	t.Cleanup(r.Close)
	return h
}

func (h *harness) live() {
	h.r.Start()
	h.mt.push(meetings.Update{Meeting: h.m})
	h.eventually("session", func() bool { _, ok := h.r.session(h.m.ID); return ok })
}

func (h *harness) line(typ, who, text string, final bool) {
	ev := meetings.Event{Type: typ, MeetingID: "q1", Text: text, Final: final}
	if typ == meetings.EventSegment {
		ev.Speaker = &meetings.Person{Name: who}
	} else {
		ev.Author = &meetings.Person{Name: who}
	}
	ev.Raw, _ = json.Marshal(ev)
	h.mt.push(meetings.Update{Meeting: h.m, Event: &ev})
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	testutil.Eventually(h.t, 5*time.Second, cond, "timed out waiting for %s", what)
}

func (h *harness) state() State {
	st, err := h.r.State(h.m.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func TestNewRunnerRequiresDeps(t *testing.T) {
	if _, err := NewRunner(Options{}); err == nil {
		t.Fatal("want an error")
	}
}

func TestForwardingAndAnswers(t *testing.T) {
	h := newHarness(t, Policy{ChatPostsPerMinute: 10}, nil)
	h.live()
	h.line(meetings.EventSegment, "Ana", "partial", false)
	h.line(meetings.EventSegment, "Ana", "we ship friday", true)
	h.mt.push(meetings.Update{Meeting: h.m}) // no event: ignored
	h.eventually("segment forwarded", func() bool { return h.spek.has("we ship friday") })
	if h.spek.has("partial") {
		t.Fatal("only final segments reach spek")
	}
	h.spek.intent(`{"type":"ask","qid":"q-1","text":"Which friday?"}`)
	h.eventually("ask posted", func() bool { p, _ := h.mt.got(); return len(p) == 1 })
	h.line(meetings.EventChat, "Bo", "the 20th", false)
	h.eventually("answer", func() bool { return h.spek.has(`"qid":"q-1"`) && h.spek.has(`"type":"answer"`) })
	st := h.state()
	if len(st.Asks) != 1 || st.Asks[0].AnsweredBy != "Bo" || st.Asks[0].Answer != "the 20th" {
		t.Fatalf("asks %+v", st.Asks)
	}
	h.spek.intent(`{"type":"heartbeat"}`)
	h.spek.intent(`not json`)
	h.spek.intent(`{"type":"mystery"}`)
	h.eventually("heartbeat", func() bool { return h.state().LastHeartbeat != nil })
}

func TestAskExpires(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := newHarness(t, Policy{AnswerWindow: time.Minute}, func(o *Options) { o.Now = clock })
	h.live()
	h.spek.intent(`{"type":"ask","qid":"q-1","text":"why?"}`)
	h.eventually("ask", func() bool { return len(h.state().Asks) == 1 })
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	h.line(meetings.EventChat, "Bo", "unrelated", false)
	h.eventually("expired", func() bool { a := h.state().Asks; return len(a) == 1 && a[0].Expired })
	if h.spek.has(`"type":"answer"`) {
		t.Fatal("an expired ask must not be answered")
	}
}

func TestChatRateLimitAndTruncation(t *testing.T) {
	h := newHarness(t, Policy{ChatPostsPerMinute: 2}, nil)
	h.live()
	long := strings.Repeat("x", ChatMaxChars+10)
	h.spek.intent(`{"type":"chat.post","text":"` + long + `"}`)
	h.spek.intent(`{"type":"chat.post","text":"two"}`)
	h.spek.intent(`{"type":"chat.post","text":"three"}`)
	h.spek.intent(`{"type":"chat.post","text":"   "}`)
	h.eventually("rate limit", func() bool { return h.state().Dropped == 1 })
	h.eventually("two posts out", func() bool { p, _ := h.mt.got(); return len(p) == 2 })
	posts, _ := h.mt.got()
	if len(posts) != 2 || len([]rune(posts[0])) != ChatMaxChars {
		t.Fatalf("posts %d, first %d runes", len(posts), len([]rune(posts[0])))
	}
}

func TestChatRefusedBeforeConsentIsDropped(t *testing.T) {
	h := newHarness(t, Policy{}, nil)
	h.mt.postErr = meetings.ErrConsentPending
	h.live()
	h.spek.intent(`{"type":"chat.post","text":"hello"}`)
	h.eventually("dropped", func() bool { return h.state().Dropped == 1 })
}

func TestNotesUpdatedInPlace(t *testing.T) {
	h := newHarness(t, Policy{}, nil)
	h.live()
	var events []Event
	var mu sync.Mutex
	h.r.Observe(func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() })
	h.spek.intent(`{"type":"notes.append","section":"decisions","text":"ship friday"}`)
	h.spek.intent(`{"type":"notes.append","section":"bogus","text":"misc"}`)
	h.spek.intent(`{"type":"notes.append","section":"actions","text":"  "}`)
	h.spek.intent(`{"type":"notes.append","section":"summary","text":"short sync"}`)
	h.eventually("notes", func() bool { return len(h.state().Notes[SectionSummary]) == 1 })
	if h.notes.count() != 1 {
		t.Fatalf("want one fact per meeting, got %d", h.notes.count())
	}
	h.notes.mu.Lock()
	body := h.notes.facts[NotesTitle(h.m)]
	h.notes.mu.Unlock()
	for _, want := range []string{"## Decisions", "- ship friday", "## Notes", "- misc", "## Summary", "google meet meeting 2026-11-15 (mtg-1)"} {
		if !strings.Contains(body, want) {
			t.Errorf("notes body missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "## Summary") > strings.Index(body, "## Decisions") {
		t.Fatal("summary renders first")
	}
	mu.Lock()
	n := len(events)
	mu.Unlock()
	if n == 0 {
		t.Fatal("observers saw nothing")
	}
	titled := meetings.Meeting{ID: "mtg-2", Title: "Standup"}
	if !strings.HasPrefix(NotesTitle(titled), "Meeting notes: Standup ") {
		t.Fatal(NotesTitle(titled))
	}
}

func TestNotesSinkFailureAndAbsence(t *testing.T) {
	h := newHarness(t, Policy{}, nil)
	h.notes.err = errors.New("vault down")
	h.live()
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"x"}`)
	h.eventually("note kept", func() bool { return len(h.state().Notes[SectionNotes]) == 1 })

	h2 := newHarness(t, Policy{}, func(o *Options) { o.Notes = nil })
	h2.live()
	h2.spek.intent(`{"type":"notes.append","section":"notes","text":"x"}`)
	h2.eventually("note kept", func() bool { return len(h2.state().Notes[SectionNotes]) == 1 })
}

func TestResearchCapApprovalAndSignals(t *testing.T) {
	h := newHarness(t, Policy{ResearchPerMeeting: 1, ChatPostsPerMinute: 1}, nil)
	h.live()
	h.spek.intent(`{"type":"research","rid":"r1","query":"vexa latency","reason":"asked"}`)
	h.spek.intent(`{"type":"research","rid":"r1","query":"dup"}`)
	h.spek.intent(`{"type":"research","rid":"","query":"x"}`)
	h.spek.intent(`{"type":"research","rid":"r2","query":"zoom guest policy"}`)
	h.eventually("two items", func() bool { return len(h.state().Research) == 2 })
	st := h.state()
	if st.Research[0].Status != ResearchQueued || st.Research[0].KickID != "kick-r1" || st.Research[1].Status != ResearchPendingApproval {
		t.Fatalf("research %+v", st.Research)
	}
	if st.Dropped != 2 {
		t.Fatalf("dropped %d", st.Dropped)
	}
	if h.res.metas[0]["untrusted"] != "true" || h.res.metas[0]["meeting_id"] != "mtg-1" {
		t.Fatalf("meta %+v", h.res.metas[0])
	}
	if !h.spek.has("waiting for approval") {
		t.Fatal("spek must hear the item is waiting")
	}
	if err := h.r.Approve(h.m.ID, "nope"); !errors.Is(err, ErrUnknownResearch) {
		t.Fatalf("unknown = %v", err)
	}
	if err := h.r.Approve(h.m.ID, "r1"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("not pending = %v", err)
	}
	if err := h.r.Approve(h.m.ID, "r2"); err != nil {
		t.Fatal(err)
	}
	if err := h.r.Approve("mtg-none", "r2"); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("no session = %v", err)
	}

	if h.r.DeliverResearch(ResearchSignal{KickID: "kick-unknown", Kind: SignalFinding}) {
		t.Fatal("unknown kicks are not delivered")
	}
	h.r.DeliverResearch(ResearchSignal{KickID: "kick-r1", Kind: SignalFinding, Text: "p50 is 900ms"})
	h.r.DeliverResearch(ResearchSignal{KickID: "kick-r1", Kind: "status", Text: "ignored"})
	h.r.DeliverResearch(ResearchSignal{KickID: "kick-r1", Kind: SignalComplete, Text: "about a second"})
	h.eventually("done", func() bool { return h.state().Research[0].Status == ResearchDone })
	h.eventually("summary posted", func() bool {
		p, _ := h.mt.got()
		for _, x := range p {
			if strings.Contains(x, `Research on "vexa latency": about a second`) {
				return true
			}
		}
		return false
	})
	if !h.spek.has(`"status":"running"`) || !h.spek.has(`"status":"done"`) {
		t.Fatalf("research updates missing: %v", h.spek.stdin())
	}
	h.eventually("research fact", func() bool { return h.notes.count() == 1 })
	if h.r.DeliverResearch(ResearchSignal{KickID: "kick-r1", Kind: SignalComplete}) {
		t.Fatal("a completed kick is unregistered")
	}
	h.r.DeliverResearch(ResearchSignal{KickID: "kick-r2", Kind: SignalError, Text: "blocked"})
	h.eventually("failed", func() bool { return h.state().Research[1].Status == ResearchFailed })
}

func TestResearchSummaryFallsBackToFinding(t *testing.T) {
	h := newHarness(t, Policy{}, nil)
	h.live()
	h.spek.intent(`{"type":"research","rid":"r1","query":"q"}`)
	h.eventually("queued", func() bool { return len(h.state().Research) == 1 })
	h.r.DeliverResearch(ResearchSignal{KickID: "kick-r1", Kind: SignalFinding, Text: "the finding"})
	h.r.DeliverResearch(ResearchSignal{KickID: "kick-r1", Kind: SignalComplete})
	h.eventually("posted", func() bool { p, _ := h.mt.got(); return len(p) == 1 && strings.Contains(p[0], "the finding") })
	if !strings.Contains(renderResearch(&ResearchItem{Query: "q", Reason: "r", Summary: "s", Findings: []string{"f"}}), "Reason: r") {
		t.Fatal("render")
	}
}

func TestResearchDispatchFailures(t *testing.T) {
	h := newHarness(t, Policy{}, nil)
	h.res.err = errors.New("pool busy")
	h.live()
	h.spek.intent(`{"type":"research","rid":"r1","query":"q"}`)
	h.eventually("failed", func() bool { r := h.state().Research; return len(r) == 1 && r[0].Status == ResearchFailed })

	h2 := newHarness(t, Policy{}, func(o *Options) { o.Research = nil })
	h2.live()
	h2.spek.intent(`{"type":"research","rid":"r1","query":"q"}`)
	h2.eventually("unavailable", func() bool { r := h2.state().Research; return len(r) == 1 && r[0].Status == ResearchFailed })
}

func TestSayPolicy(t *testing.T) {
	h := newHarness(t, Policy{VoiceOnAddress: true}, nil)
	h.m.VoiceEnabled = true
	h.mt.m[h.m.ID] = h.m
	h.live()
	h.spek.intent(`{"type":"say","text":"spoken"}`)
	h.eventually("spoken", func() bool { _, s := h.mt.got(); return len(s) == 1 })
	h.mt.mu.Lock()
	h.mt.sayErr = errors.New("platform_unavailable")
	h.mt.mu.Unlock()
	h.spek.intent(`{"type":"say","text":"fallback"}`)
	// A say call that failed: the line goes to chat and says why it is there.
	h.eventually("fallback to chat", func() bool { p, _ := h.mt.got(); return len(p) == 1 && p[0] == sayFailedPrefix+"fallback" })

	h2 := newHarness(t, Policy{VoiceOnAddress: false}, nil)
	h2.m.VoiceEnabled = true
	h2.mt.m[h2.m.ID] = h2.m
	h2.live()
	h2.spek.intent(`{"type":"say","text":"downgraded"}`)
	h2.eventually("downgraded", func() bool { p, s := h2.mt.got(); return len(p) == 1 && len(s) == 0 })
}

func TestBudgetByIntentsAndTokens(t *testing.T) {
	h := newHarness(t, Policy{MaxIntentsPerMeeting: 1}, nil)
	h.live()
	h.spek.intent(`{"type":"chat.post","text":"one"}`)
	h.spek.intent(`{"type":"chat.post","text":"two"}`)
	h.spek.intent(`{"type":"ask","qid":"a","text":"x"}`)
	h.spek.intent(`{"type":"research","rid":"r","query":"x"}`)
	h.spek.intent(`{"type":"say","text":"x"}`)
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"still noted"}`)
	h.eventually("notes still taken", func() bool { return len(h.state().Notes[SectionNotes]) == 1 })
	h.eventually("post and notice out", func() bool { p, _ := h.mt.got(); return len(p) == 2 })
	st := h.state()
	posts, _ := h.mt.got()
	if !st.BudgetExhausted || len(posts) != 2 || posts[1] != budgetNotice(defaultPersonaName) || st.Dropped != 4 {
		t.Fatalf("budget state %+v posts %q", st, posts)
	}

	h2 := newHarness(t, Policy{MaxTokensPerMeeting: 100}, nil)
	h2.live()
	h2.spek.intent(`{"type":"chat.post","text":"a","usage":{"input_tokens":60,"output_tokens":50}}`)
	h2.eventually("token budget", func() bool { return h2.state().BudgetExhausted })
	if h2.state().Tokens != 110 {
		t.Fatalf("tokens %d", h2.state().Tokens)
	}

	ledger := NewLedger(50, 0)
	h3 := newHarness(t, Policy{}, func(o *Options) { o.Ledger = ledger })
	h3.live()
	h3.spek.intent(`{"type":"chat.post","text":"a","usage":{"total_tokens":60}}`)
	h3.eventually("hive-wide ceiling", func() bool { return h3.state().BudgetExhausted })
}

func TestLedgerAndHelpers(t *testing.T) {
	var nilLedger *Ledger
	nilLedger.Add(time.Now(), 5)
	if nilLedger.Exceeded(time.Now()) {
		t.Fatal("nil ledger never exceeds")
	}
	now := time.Now()
	l := NewLedger(10, 15)
	l.Add(now.Add(-25*time.Hour), 100)
	l.Add(now.Add(-2*time.Hour), 9)
	l.Add(now, 0)
	if l.Exceeded(now) {
		t.Fatal("old and zero entries do not count")
	}
	l.Add(now, 6)
	if !l.Exceeded(now) {
		t.Fatal("daily ceiling reached")
	}
	r := rateLimiter{}
	if !r.allow(now) {
		t.Fatal("zero limit allows all")
	}
	if truncate("abc", 5) != "abc" || truncate("abcdef", 4) != "abc…" {
		t.Fatal("truncate")
	}
}

func TestEndSendsEndAndPersists(t *testing.T) {
	h := newHarness(t, Policy{}, nil)
	h.live()
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"before end"}`)
	h.eventually("note", func() bool { return len(h.state().Notes[SectionNotes]) == 1 })
	ended := h.m
	ended.State = meetings.StateEnded
	h.mt.push(meetings.Update{Meeting: ended})
	h.mt.push(meetings.Update{Meeting: ended})
	h.eventually("session gone", func() bool { _, ok := h.r.session(h.m.ID); return !ok })
	if !h.spek.has(`{"type":"end"}`) {
		t.Fatal("spek must get end")
	}
	st, err := h.r.State(h.m.ID)
	if err != nil || len(st.Notes[SectionNotes]) != 1 {
		t.Fatalf("stored state %+v, %v", st, err)
	}
	if _, err := h.r.State("mtg-none"); !errors.Is(err, meetings.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestSpekExitKeepsSessionDegraded(t *testing.T) {
	h := newHarness(t, Policy{}, func(o *Options) { o.SpekMode = "rules only, no model configured" })
	h.live()
	if st := h.state(); st.SpekMode != "rules only, no model configured" {
		t.Fatalf("spek mode %q", st.SpekMode)
	}
	h.spek.Close(nil)
	h.eventually("degraded", func() bool {
		return strings.Contains(h.state().SpekStatus, "spektacular/pull/126") || strings.Contains(h.state().SpekStatus, "#126")
	})
	// Research signals and approvals still reach the session.
	if _, ok := h.r.session(h.m.ID); !ok {
		t.Fatal("the session must outlive Spek")
	}
	ended := h.m
	ended.State = meetings.StateEnded
	h.mt.push(meetings.Update{Meeting: ended})
	h.eventually("session gone", func() bool { _, ok := h.r.session(h.m.ID); return !ok })
}

// TestSpekDownSaidPlainly pins hivecommons/hive#18590 finding 8: while
// hive cannot reach Spektacular the panel says notes and clarifying
// questions are off, and clears it when Spek writes again.
func TestSpekDownSaidPlainly(t *testing.T) {
	h := newHarness(t, Policy{}, nil)
	h.live()
	h.spek.mu.Lock()
	h.spek.sendErr = errors.New("spektacular: stream child is not running")
	h.spek.mu.Unlock()
	h.line(meetings.EventSegment, "Ana", "let's start", true)
	h.eventually("paused status", func() bool {
		return strings.HasPrefix(h.state().SpekStatus, "Notes and clarifying questions are paused")
	})
	h.spek.mu.Lock()
	h.spek.sendErr = nil
	h.spek.mu.Unlock()
	h.spek.intent(`{"type":"heartbeat"}`)
	h.eventually("cleared", func() bool { return h.state().SpekStatus == "" })
	h.spek.Close(nil)
	h.eventually("stopped status", func() bool {
		st := h.state().SpekStatus
		return strings.HasPrefix(st, "Notes and clarifying questions are off for this meeting") && strings.Contains(st, "#126")
	})
}

func TestUsageFileCharged(t *testing.T) {
	usage := filepath.Join(t.TempDir(), "usage.jsonl")
	h := newHarness(t, Policy{MaxTokensPerMeeting: 50}, func(o *Options) { o.UsageFile = func(string) string { return usage } })
	h.live()
	if err := os.WriteFile(usage, []byte("{\"tokens\":30}\n{\"tokens\":25}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"n"}`)
	h.eventually("tokens from the usage file", func() bool { st := h.state(); return st.Tokens == 55 && st.BudgetExhausted })
}

func TestSpekStartFailureAndResume(t *testing.T) {
	st, _ := meetings.NewStore(t.TempDir())
	m := meetings.Meeting{ID: "mtg-r", State: meetings.StateLive}
	_ = st.Save(m)
	_ = st.SaveDoc(m.ID, StateDoc, State{Research: []ResearchItem{{RID: "r1", KickID: "k1", Status: ResearchRunning}}})
	mt := &fakeMeetings{m: map[string]meetings.Meeting{m.ID: m}}
	bad := newFakeSpek()
	bad.startErr = errors.New("no binary")
	r, _ := NewRunner(Options{Meetings: mt, Store: st, NewSpek: func(meetings.Meeting) Spek { return bad }})
	r.Start()
	if _, ok := r.session(m.ID); ok {
		t.Fatal("a spek that cannot start gives no session")
	}
	r.Close()

	good := newFakeSpek()
	r2, _ := NewRunner(Options{Meetings: mt, Store: st, NewSpek: func(meetings.Meeting) Spek { return good }})
	defer r2.Close()
	r2.Start()
	r2.Start()
	testutil.Eventually(t, 5*time.Second, func() bool { _, ok := r2.session(m.ID); return ok }, "resumed session")
	if !r2.DeliverResearch(ResearchSignal{KickID: "k1", Kind: SignalComplete, Text: "done"}) {
		t.Fatal("in-flight research kicks are re-registered on resume")
	}
}

func TestCorruptStateStartsFresh(t *testing.T) {
	h := newHarness(t, Policy{}, nil)
	_ = h.store.SaveDoc(h.m.ID, StateDoc, map[string]any{"notes": "not a map"})
	h.live()
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"n"}`)
	h.eventually("fresh state accepts notes", func() bool { return len(h.state().Notes[SectionNotes]) == 1 })
}

type fakeImporter struct {
	layer   knowledge.LayerType
	content string
	err     error
}

func (f *fakeImporter) ImportFacts(_ context.Context, layer knowledge.LayerType, content, format string) (int, error) {
	f.layer, f.content = layer, content
	return 1, f.err
}

func TestKnowledgeNotes(t *testing.T) {
	imp := &fakeImporter{}
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := (KnowledgeNotes{API: imp, Now: func() time.Time { return fixed }}).Upsert(context.Background(), "T", "B", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	var facts []knowledge.ExtractedFact
	if err := json.Unmarshal([]byte(imp.content), &facts); err != nil {
		t.Fatal(err)
	}
	if imp.layer != DefaultVault || len(facts) != 1 || facts[0].Title != "T" || !facts[0].Sensitive || !facts[0].SourceDate.Equal(fixed) {
		t.Fatalf("import %s %+v", imp.layer, facts)
	}
	imp.err = errors.New("no channel")
	if err := (KnowledgeNotes{API: imp, Vault: "other"}).Upsert(context.Background(), "T", "B", nil); err == nil || imp.layer != "other" {
		t.Fatal("errors and vault override")
	}
}

// earlyDispatcher reports a finding and completion before Dispatch returns.
type earlyDispatcher struct{ r **Runner }

func (d earlyDispatcher) Dispatch(_ context.Context, _ string, meta map[string]string) (string, error) {
	kick := "kick-" + meta["rid"]
	(*d.r).DeliverResearch(ResearchSignal{KickID: kick, Kind: SignalFinding, Text: "early finding"})
	(*d.r).DeliverResearch(ResearchSignal{KickID: kick, Kind: SignalComplete, Text: "early summary"})
	return kick, nil
}

func TestSignalsBeforeDispatchReturns(t *testing.T) {
	var runner *Runner
	h := newHarness(t, Policy{}, func(o *Options) { o.Research = earlyDispatcher{r: &runner} })
	runner = h.r
	h.live()
	h.spek.intent(`{"type":"research","rid":"r1","query":"fast"}`)
	h.eventually("early signals applied", func() bool {
		st := h.state()
		return len(st.Research) == 1 && st.Research[0].Status == ResearchDone && st.Research[0].Summary == "early summary"
	})
	for i := 0; i < maxEarlyKicks+2; i++ {
		h.r.DeliverResearch(ResearchSignal{KickID: "orphan-" + strconv.Itoa(i), Kind: SignalFinding})
	}
	h.r.mu.Lock()
	n := len(h.r.early)
	h.r.mu.Unlock()
	if n != maxEarlyKicks {
		t.Fatalf("early buffer holds %d kicks, want %d", n, maxEarlyKicks)
	}
	h.r.registerKick("orphan-unknown-meeting", "mtg-none")
}
