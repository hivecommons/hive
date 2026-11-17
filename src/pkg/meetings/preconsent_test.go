package meetings

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedQuorum holds the consent notice back (a transient rate limit) until
// open is set, so a test can send lines while consent is pending.
type gatedQuorum struct {
	*fakeQuorum
	open   atomic.Bool
	cursor int64 // cursorQuorum's transcript.read next_seq
}

var errSlowDown = &ToolError{Tool: "chat.post", Code: CodeRateLimited, Message: "slow down"}

func (g *gatedQuorum) ChatPostConfirmed(_ context.Context, _ string, text string) (bool, error) {
	if !g.open.Load() {
		return false, errSlowDown
	}
	g.fakeQuorum.mu.Lock()
	g.fakeQuorum.posts = append(g.fakeQuorum.posts, text)
	g.fakeQuorum.mu.Unlock()
	return true, nil
}

// cursorQuorum is a gatedQuorum that also reports Quorum's transcript cursor.
type cursorQuorum struct{ *gatedQuorum }

func (c cursorQuorum) TranscriptRead(context.Context, string, int64) (TranscriptResult, error) {
	return TranscriptResult{NextSeq: c.cursor}, nil
}

// recorder keeps every update's event text a listener saw.
type recorder struct {
	mu    sync.Mutex
	texts []string
}

func (r *recorder) on(u Update) {
	if u.Event == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.texts = append(r.texts, u.Event.Text)
}

func (r *recorder) saw(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.texts {
		if t == text {
			return true
		}
	}
	return false
}

func newPendingManager(t *testing.T, st *Store, q Quorum) *Manager {
	t.Helper()
	m, err := NewManager(Options{
		Store: st, Quorum: q, BotName: "Betty Bee from the hive", ConsentText: testConsent,
		LeavePhrases: []string{"betty leave", "hive leave"},
		ReconnectMin: 5 * time.Millisecond, ReconnectMax: 20 * time.Millisecond,
		ConsentRetry: time.Millisecond, ConsentAttempts: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// goLive joins a meeting and takes it live with consent still pending.
func goLive(t *testing.T, m *Manager, q *fakeQuorum) Meeting {
	t.Helper()
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/abc-defg-hij"})
	if err != nil {
		t.Fatal(err)
	}
	q.stream("q1") <- Event{Type: EventState, State: StateLive}
	eventually(t, "live with consent pending", func() bool {
		got, _ := m.Get(mt.ID)
		return got.State == StateLive && got.ConsentError != ""
	})
	return mt
}

func dropped(m *Manager, id string, n int) func() bool {
	return func() bool { got, _ := m.Get(id); return got.PreConsentDropped >= n }
}

func TestPreConsentLinesNeitherStoredNorForwarded(t *testing.T) {
	g := &gatedQuorum{fakeQuorum: newFakeQuorum()}
	m := newPendingManager(t, newStore(t), g)
	rec := &recorder{}
	defer m.Subscribe(rec.on)()
	mt := goLive(t, m, g.fakeQuorum)
	ch := g.stream("q1")

	ch <- Event{Type: EventSegment, Seq: 1, Speaker: &Person{Name: "Ana"}, Text: "secret draft", Final: false}
	ch <- Event{Type: EventSegment, Seq: 2, Speaker: &Person{Name: "Ana"}, Text: "secret plan", Final: true}
	ch <- Event{Type: EventChat, Seq: 3, Author: &Person{Name: "Bo"}, Text: "secret chat"}
	eventually(t, "three drops", dropped(m, mt.ID, 3))

	// The notice gets through on the next line; that line still came
	// before it and is dropped too.
	// One line, sent once: it is what gets the notice through. Sending
	// lines on a timer until consent shows raced the watcher (a second line
	// handled after consent was posted is a legitimate post-consent line).
	g.open.Store(true)
	const lateSeq, afterSeq = int64(4), int64(5)
	ch <- Event{Type: EventChat, Seq: lateSeq, Author: &Person{Name: "Bo"}, Text: "secret late"}
	eventually(t, "consent delivered", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	eventually(t, "late line dropped", dropped(m, mt.ID, 4))
	seq := afterSeq
	ch <- Event{Type: EventSegment, Seq: seq, Speaker: &Person{Name: "Ana"}, Text: "after consent", Final: true}
	eventually(t, "post-consent line stored", func() bool { tr, _ := heardLines(m, mt.ID); return len(tr) == 1 })
	// The listener is told after the store write and a read, and the cursor
	// is saved after the write: wait for both before asserting on them.
	eventually(t, "post-consent line reached listeners", func() bool { return rec.saw("after consent") })
	eventually(t, "cursor moved", func() bool { got, _ := m.Get(mt.ID); return got.LastSeq == seq })

	tr, _ := heardLines(m, mt.ID)
	if tr[0].Text != "after consent" {
		t.Fatalf("transcript %+v", tr)
	}
	for _, s := range []string{"secret draft", "secret plan", "secret chat", "secret late"} {
		if rec.saw(s) {
			t.Fatalf("listener saw pre-consent line %q", s)
		}
	}
	if !rec.saw("after consent") {
		t.Fatal("the post-consent line must reach listeners")
	}
	got, _ := m.Get(mt.ID)
	if got.PreConsentDropped < 4 || got.LastSeq != seq || got.State != StateLive {
		t.Fatalf("meeting %+v", got)
	}
	// Only the count is kept: nothing of the content is in the record.
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "secret") {
		t.Fatalf("pre-consent content in the meeting record: %s", b)
	}
}

func TestPreConsentLeaveHonoured(t *testing.T) {
	g := &gatedQuorum{fakeQuorum: newFakeQuorum()}
	m := newPendingManager(t, newStore(t), g)
	rec := &recorder{}
	defer m.Subscribe(rec.on)()
	mt := goLive(t, m, g.fakeQuorum)

	g.stream("q1") <- Event{Type: EventChat, Seq: 1, Author: &Person{Name: "Ana"}, Text: "Betty, please leave"}
	eventually(t, "left", stateIs(m, mt.ID, StateEnded))
	got, _ := m.Get(mt.ID)
	if got.StateReason != ReasonHiveLeave || got.ConsentPostedAt != nil || got.PreConsentDropped != 1 {
		t.Fatalf("meeting %+v", got)
	}
	if _, leaves := g.snapshot(); len(leaves) != 1 || leaves[0] != "q1" {
		t.Fatalf("leaves %q", leaves)
	}
	if tr, _ := heardLines(m, mt.ID); len(tr) != 0 {
		t.Fatalf("the leave line was stored: %+v", tr)
	}
	if rec.saw("Betty, please leave") {
		t.Fatal("the leave line was forwarded")
	}
}

func TestPreConsentBotOwnNoticeIgnored(t *testing.T) {
	g := &gatedQuorum{fakeQuorum: newFakeQuorum()}
	m := newPendingManager(t, newStore(t), g)
	mt := goLive(t, m, g.fakeQuorum)
	// The notice itself says "hive leave"; its echo must not remove the bot
	// or count as a participant's line.
	g.stream("q1") <- Event{Type: EventChat, Seq: 1, Author: &Person{Name: "Betty Bee from the hive (Guest)"}, Text: testConsent}
	g.stream("q1") <- Event{Type: EventChat, Seq: 2, Author: &Person{Name: "Someone"}, Text: testConsent}
	g.stream("q1") <- Event{Type: EventChat, Seq: 3, Author: &Person{Name: "Ana"}, Text: "hi"}
	eventually(t, "a participant line dropped", dropped(m, mt.ID, 1))
	got, _ := m.Get(mt.ID)
	if got.State != StateLive || got.PreConsentDropped != 1 {
		t.Fatalf("meeting %+v", got)
	}
}

func TestPreConsentNoReplayOnReconnectOrRestart(t *testing.T) {
	st := newStore(t)
	g := &gatedQuorum{fakeQuorum: newFakeQuorum()}
	m := newPendingManager(t, st, g)
	mt := goLive(t, m, g.fakeQuorum)
	ch := g.stream("q1")
	ch <- Event{Type: EventSegment, Seq: 5, Speaker: &Person{Name: "Ana"}, Text: "secret", Final: true}
	eventually(t, "drop", dropped(m, mt.ID, 1))
	if got, _ := m.Get(mt.ID); got.LastSeq != 5 {
		t.Fatalf("the cursor must move past a dropped line: %+v", got)
	}
	// A reconnect while consent is pending asks for no replay.
	close(ch)
	eventually(t, "reconnect", func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.connects >= 2 })
	g.mu.Lock()
	if last := g.afters[len(g.afters)-1]; last != 0 {
		g.mu.Unlock()
		t.Fatalf("reconnect while pending replayed from %d", last)
	}
	g.mu.Unlock()
	m.Close()

	// A restart while pending: same, and the first line after the notice is
	// the first one kept.
	g2 := &gatedQuorum{fakeQuorum: newFakeQuorum()}
	m2 := newPendingManager(t, st, g2)
	if n := m2.Resume(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	eventually(t, "resume connect", func() bool { g2.mu.Lock(); defer g2.mu.Unlock(); return g2.connects >= 1 })
	g2.mu.Lock()
	if g2.afters[0] != 0 {
		g2.mu.Unlock()
		t.Fatalf("resume while pending replayed from %d", g2.afters[0])
	}
	g2.mu.Unlock()
	ch2 := g2.stream("q1")
	g2.open.Store(true)
	ch2 <- Event{Type: EventState, State: StateLive}
	eventually(t, "consent after resume", func() bool { got, _ := m2.Get(mt.ID); return got.ConsentPostedAt != nil })
	ch2 <- Event{Type: EventSegment, Seq: 4, Speaker: &Person{Name: "Ana"}, Text: "old", Final: true}
	ch2 <- Event{Type: EventSegment, Seq: 7, Speaker: &Person{Name: "Ana"}, Text: "kept", Final: true}
	eventually(t, "kept", func() bool { tr, _ := heardLines(m2, mt.ID); return len(tr) == 1 })
	if tr, _ := heardLines(m2, mt.ID); tr[0].Text != "kept" {
		t.Fatalf("transcript %+v", tr)
	}
}

func TestConsentCursorFence(t *testing.T) {
	g := &gatedQuorum{fakeQuorum: newFakeQuorum(), cursor: 9}
	g.open.Store(true)
	m := newPendingManager(t, newStore(t), cursorQuorum{g})
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	ch := g.stream("q1")
	ch <- Event{Type: EventState, State: StateLive}
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	if got, _ := m.Get(mt.ID); got.ConsentSeq != 9 {
		t.Fatalf("consent seq %d, want quorum's cursor 9", got.ConsentSeq)
	}
	// Quorum had seq 8 before the notice landed; it arrives after it.
	ch <- Event{Type: EventSegment, Seq: 8, Speaker: &Person{Name: "Ana"}, Text: "before", Final: true}
	ch <- Event{Type: EventSegment, Seq: 10, Speaker: &Person{Name: "Ana"}, Text: "after", Final: true}
	eventually(t, "after stored", func() bool { tr, _ := heardLines(m, mt.ID); return len(tr) == 1 })
	if tr, _ := heardLines(m, mt.ID); tr[0].Text != "after" {
		t.Fatalf("transcript %+v", tr)
	}
	if got, _ := m.Get(mt.ID); got.PreConsentDropped != 1 {
		t.Fatalf("dropped %d", got.PreConsentDropped)
	}
}

func TestBeforeConsent(t *testing.T) {
	posted := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	mt := Meeting{ConsentPostedAt: &posted, ConsentSeq: 5, ConsentSpeechEnd: 30}
	soon := posted.Add(time.Second)
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := []struct {
		name string
		mt   Meeting
		ev   Event
		now  time.Time
		want bool
	}{
		{"pending", Meeting{}, Event{Type: EventChat, Seq: 99}, soon, true},
		{"seq at fence", mt, Event{Type: EventSegment, Seq: 5, Final: true}, soon, true},
		{"seq after fence", mt, Event{Type: EventSegment, Seq: 6, Final: true, Start: raw("31")}, soon, false},
		{"chat stamped before", mt, Event{Type: EventChat, Seq: 6, At: raw(`"2026-11-16T09:59:59Z"`)}, soon, true},
		{"chat stamped after", mt, Event{Type: EventChat, Seq: 6, At: raw(`"2026-11-16T10:00:01Z"`)}, soon, false},
		{"chat unstamped", mt, Event{Type: EventChat, Seq: 6}, soon, false},
		{"chat bad stamp", mt, Event{Type: EventChat, Seq: 6, At: raw(`"yesterday"`)}, soon, false},
		{"segment began before speech end", mt, Event{Type: EventSegment, Seq: 6, Final: true, Start: raw("29.5")}, soon, true},
		{"segment began before, window over", mt, Event{Type: EventSegment, Seq: 6, Final: true, Start: raw("1")}, posted.Add(ConsentSpeechWindow), false},
		{"segment without start", mt, Event{Type: EventSegment, Seq: 6, Final: true}, soon, false},
		// No line was heard while consent was pending (ConsentSpeechEnd 0):
		// an utterance 4s long that arrives 1s after the notice began before it.
		{"long utterance finalized just after the notice", Meeting{ConsentPostedAt: &posted, ConsentSeq: 5}, Event{Type: EventSegment, Seq: 6, Final: true, Start: raw("10"), End: raw("14")}, soon, true},
		{"short utterance well after the notice", Meeting{ConsentPostedAt: &posted, ConsentSeq: 5}, Event{Type: EventSegment, Seq: 6, Final: true, Start: raw("10"), End: raw("11")}, posted.Add(ConsentSTTLag + 2*time.Second), false},
		{"unreadable cursor: any segment soon after", Meeting{ConsentPostedAt: &posted, ConsentFenceDegraded: true}, Event{Type: EventSegment, Seq: 6, Final: true}, soon, true},
		{"unreadable cursor: after the window", Meeting{ConsentPostedAt: &posted, ConsentFenceDegraded: true}, Event{Type: EventSegment, Seq: 6, Final: true}, posted.Add(ConsentDegradedWindow), false},
		{"unreadable cursor: chat is judged by its stamp", Meeting{ConsentPostedAt: &posted, ConsentFenceDegraded: true}, Event{Type: EventChat, Seq: 6}, soon, false},
	}
	for _, c := range cases {
		if got := beforeConsent(c.mt, c.ev, c.now); got != c.want {
			t.Errorf("%s: beforeConsent = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPreConsentSpeechEndFence(t *testing.T) {
	g := &gatedQuorum{fakeQuorum: newFakeQuorum()}
	m := newPendingManager(t, newStore(t), g)
	mt := goLive(t, m, g.fakeQuorum)
	ch := g.stream("q1")
	ch <- Event{Type: EventSegment, Seq: 1, Speaker: &Person{Name: "Ana"}, Text: "secret", Final: false,
		Start: json.RawMessage("10"), End: json.RawMessage("12.5")}
	eventually(t, "drop", dropped(m, mt.ID, 1))
	if got, _ := m.Get(mt.ID); got.ConsentSpeechEnd != 12.5 || got.ConsentSeq != 1 {
		t.Fatalf("fences %+v", got)
	}
}

// failingCursorQuorum cannot read Quorum's transcript cursor.
type failingCursorQuorum struct {
	*gatedQuorum
	reads atomic.Int32
}

func (f *failingCursorQuorum) TranscriptRead(context.Context, string, int64) (TranscriptResult, error) {
	f.reads.Add(1)
	return TranscriptResult{}, errSlowDown
}

func TestConsentCursorReadFailureFailsClosed(t *testing.T) {
	g := &gatedQuorum{fakeQuorum: newFakeQuorum()}
	g.open.Store(true)
	q := &failingCursorQuorum{gatedQuorum: g}
	m := newPendingManager(t, newStore(t), q)
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	ch := g.stream("q1")
	ch <- Event{Type: EventState, State: StateLive}
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	got, _ := m.Get(mt.ID)
	if !got.ConsentFenceDegraded || q.reads.Load() != ConsentCursorAttempts {
		t.Fatalf("degraded %v after %d reads, want a fail-closed fence after %d", got.ConsentFenceDegraded, q.reads.Load(), ConsentCursorAttempts)
	}
	// A line Quorum had buffered before the notice, arriving right after it,
	// is not stored.
	ch <- Event{Type: EventSegment, Seq: 1, Speaker: &Person{Name: "Ana"}, Text: "buffered secret", Final: true}
	eventually(t, "dropped", dropped(m, mt.ID, 1))
	if tr, _ := heardLines(m, mt.ID); len(tr) != 0 {
		t.Fatalf("a segment inside the degraded fence was stored: %+v", tr)
	}
}

func TestSegmentInProgressAtConsentNotStored(t *testing.T) {
	g := &gatedQuorum{fakeQuorum: newFakeQuorum(), cursor: 0}
	g.open.Store(true)
	m := newPendingManager(t, newStore(t), cursorQuorum{g})
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	ch := g.stream("q1")
	ch <- Event{Type: EventState, State: StateLive}
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	// Nobody finished a sentence before the notice, so no speech-end fence
	// exists; a 20s utterance finalized right after it began before it.
	ch <- Event{Type: EventSegment, Seq: 1, Speaker: &Person{Name: "Ana"}, Text: "secret in progress", Final: true,
		Start: json.RawMessage("0"), End: json.RawMessage("20")}
	eventually(t, "dropped", dropped(m, mt.ID, 1))
	if tr, _ := heardLines(m, mt.ID); len(tr) != 0 {
		t.Fatalf("speech that began before the notice was stored: %+v", tr)
	}
}
