package meetings

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// heardLines is a meeting's transcript without hive's own reply lines: what
// people said and typed.
func heardLines(m *Manager, id string) ([]Event, error) {
	tr, err := m.Transcript(id)
	out := make([]Event, 0, len(tr))
	for _, ev := range tr {
		if !ev.IsReply() {
			out = append(out, ev)
		}
	}
	return out, err
}

// replyLines is the reply lines of a meeting's transcript.
func replyLines(t *testing.T, m *Manager, id string) []Event {
	t.Helper()
	tr, err := m.Transcript(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, ev := range tr {
		if ev.IsReply() {
			out = append(out, ev)
		}
	}
	return out
}

// timedQuorum is a fakeQuorum that confirms chat, reports say timings and
// records prewarm calls and the order of what it was asked.
type timedQuorum struct {
	*fakeQuorum
	tmu        sync.Mutex
	confirm    bool
	confirmErr error
	result     SayResult
	prewarmed  [][]string
	order      []string
}

func (q *timedQuorum) ChatPostConfirmed(_ context.Context, _ string, text string) (bool, error) {
	q.tmu.Lock()
	defer q.tmu.Unlock()
	q.order = append(q.order, "chat:"+text)
	if q.confirmErr != nil {
		return false, q.confirmErr
	}
	return q.confirm, nil
}

func (q *timedQuorum) SayTimed(ctx context.Context, id, text string) (SayResult, error) {
	q.tmu.Lock()
	q.order = append(q.order, "say:"+text)
	q.tmu.Unlock()
	return q.result, q.fakeQuorum.Say(ctx, id, text)
}

func (q *timedQuorum) SayPrewarm(_ context.Context, _ string, texts []string) error {
	q.tmu.Lock()
	defer q.tmu.Unlock()
	q.prewarmed = append(q.prewarmed, texts)
	return nil
}

func (q *timedQuorum) Leave(ctx context.Context, id string) (State, error) {
	q.tmu.Lock()
	q.order = append(q.order, "leave")
	q.tmu.Unlock()
	return q.fakeQuorum.Leave(ctx, id)
}

func (q *timedQuorum) set(fn func()) {
	q.tmu.Lock()
	defer q.tmu.Unlock()
	fn()
}

func (q *timedQuorum) steps() []string {
	q.tmu.Lock()
	defer q.tmu.Unlock()
	return append([]string(nil), q.order...)
}

// liveMeeting joins a meeting and brings it live with the consent notice
// delivered.
func liveMeeting(t *testing.T, m *Manager, q *fakeQuorum, voice bool) Meeting {
	t.Helper()
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/abc-defg-hij", Voice: voice, InvitedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	q.stream("q1") <- Event{Type: EventState, MeetingID: "q1", State: StateLive}
	return waitConsent(t, m, mt.ID)
}

func TestSayReturnsQuorumsTimings(t *testing.T) {
	q := &timedQuorum{fakeQuorum: newFakeQuorum(), confirm: true, result: SayResult{DurationMS: 1800, FirstAudioMS: 240, SynthMS: 200, Cached: true}}
	m := newTestManager(t, q, time.Minute)
	mt := liveMeeting(t, m, q.fakeQuorum, true)
	got, err := m.Say(context.Background(), mt.ID, "hi")
	if err != nil || got != q.result {
		t.Fatalf("Say = %+v, %v", got, err)
	}
	// A Quorum that reports nothing answers the zero result.
	plain := newFakeQuorum()
	m2 := newTestManager(t, plain, time.Minute)
	mt2 := liveMeeting(t, m2, plain, true)
	if got, err := m2.Say(context.Background(), mt2.ID, "hi"); err != nil || got != (SayResult{}) {
		t.Fatalf("plain Say = %+v, %v", got, err)
	}
}

func TestPostChatConfirmed(t *testing.T) {
	q := &timedQuorum{fakeQuorum: newFakeQuorum(), confirm: true}
	m := newTestManager(t, q, time.Minute)
	if _, err := m.PostChatConfirmed(context.Background(), "mtg-missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	mt := liveMeeting(t, m, q.fakeQuorum, false)
	if ok, err := m.PostChatConfirmed(context.Background(), mt.ID, "hello"); err != nil || !ok {
		t.Fatalf("confirmed = %v, %v", ok, err)
	}
	q.set(func() { q.confirm = false })
	if ok, err := m.PostChatConfirmed(context.Background(), mt.ID, "lost"); err != nil || ok {
		t.Fatalf("a line the platform did not show must not count as posted: %v, %v", ok, err)
	}
	// A Quorum that cannot confirm is trusted on a nil error.
	plain := newFakeQuorum()
	m2 := newTestManager(t, plain, time.Minute)
	mt2 := liveMeeting(t, m2, plain, false)
	if ok, err := m2.PostChatConfirmed(context.Background(), mt2.ID, "hello"); err != nil || !ok {
		t.Fatalf("plain = %v, %v", ok, err)
	}
}

// A typed leave is confirmed in chat before the bot goes, and the goodbye
// and the consent notice are both in the meeting record as hive's lines.
func TestGoodbyeIsOutBeforeTheBotLeaves(t *testing.T) {
	q := &timedQuorum{fakeQuorum: newFakeQuorum(), confirm: true}
	m := newTestManager(t, q, time.Minute)
	mt := liveMeeting(t, m, q.fakeQuorum, true)
	q.stream("q1") <- Event{Type: EventChat, MeetingID: "q1", Seq: 1, Author: &Person{Name: "Bo"}, Text: "hive leave"}
	eventually(t, "ended", stateIs(m, mt.ID, StateEnded))
	steps := q.steps()
	if n := len(steps); n < 2 || steps[n-2] != "chat:"+LeavingText || steps[n-1] != "leave" {
		t.Fatalf("the goodbye must be confirmed in chat before the leave: %v", steps)
	}
	replies := replyLines(t, m, mt.ID)
	if len(replies) != 2 {
		t.Fatalf("want the consent notice and the goodbye recorded, got %+v", replies)
	}
	if r := replies[0]; r.Text != testConsent || r.Reply == nil || r.Reply.Trigger != ReplyTriggerConsent || !r.Reply.Delivered {
		t.Fatalf("consent line %+v", r)
	}
	if r := replies[1]; r.Text != LeavingText || r.Reply == nil || r.Reply.Kind != ReplyKindChat || r.Reply.Trigger != ReplyTriggerLeave || !r.Reply.Delivered {
		t.Fatalf("goodbye line %+v", r)
	}
	if heard, _ := heardLines(m, mt.ID); len(heard) != 1 || heard[0].Text != "hive leave" {
		t.Fatalf("people's lines %+v", heard)
	}
}

// A spoken leave in a meeting with voice is answered aloud, not in chat.
func TestGoodbyeIsSpokenWhenAskedByVoice(t *testing.T) {
	q := &timedQuorum{fakeQuorum: newFakeQuorum(), confirm: true}
	m := newTestManager(t, q, time.Minute)
	mt := liveMeeting(t, m, q.fakeQuorum, true)
	q.stream("q1") <- Event{Type: EventSegment, MeetingID: "q1", Seq: 1, Speaker: &Person{Name: "Bo"}, Text: "Hive leave.", Final: true}
	eventually(t, "ended", stateIs(m, mt.ID, StateEnded))
	steps := q.steps()
	if n := len(steps); n < 2 || steps[n-2] != "say:"+LeavingText || steps[n-1] != "leave" {
		t.Fatalf("the goodbye must be spoken before the leave: %v", steps)
	}
	replies := replyLines(t, m, mt.ID)
	if last := replies[len(replies)-1]; last.Reply == nil || last.Reply.Kind != ReplyKindVoice || !last.Reply.Delivered {
		t.Fatalf("goodbye line %+v", last)
	}
}

// When chat cannot carry the goodbye and the meeting has voice, it is
// spoken; when neither works it is recorded as not delivered and the bot
// still leaves.
func TestGoodbyeFallsBackAndNeverBlocksTheLeave(t *testing.T) {
	q := &timedQuorum{fakeQuorum: newFakeQuorum(), confirm: true}
	m := newTestManager(t, q, time.Minute)
	mt := liveMeeting(t, m, q.fakeQuorum, true)
	q.set(func() {
		q.confirmErr = &ToolError{Tool: "chat.post", Code: CodePlatformUnavailable, Message: "not_landed"}
	})
	q.stream("q1") <- Event{Type: EventChat, MeetingID: "q1", Seq: 1, Author: &Person{Name: "Bo"}, Text: "hive leave"}
	eventually(t, "ended", stateIs(m, mt.ID, StateEnded))
	replies := replyLines(t, m, mt.ID)
	if last := replies[len(replies)-1]; last.Reply == nil || last.Reply.Kind != ReplyKindVoice || !last.Reply.Delivered {
		t.Fatalf("a failed chat goodbye must be spoken: %+v", last)
	}

	q2 := &timedQuorum{fakeQuorum: newFakeQuorum(), confirm: true}
	m2 := newTestManager(t, q2, time.Minute)
	mt2 := liveMeeting(t, m2, q2.fakeQuorum, false)
	q2.set(func() { q2.confirm = false })
	q2.stream("q1") <- Event{Type: EventChat, MeetingID: "q1", Seq: 1, Author: &Person{Name: "Bo"}, Text: "hive leave"}
	eventually(t, "ended", stateIs(m2, mt2.ID, StateEnded))
	replies = replyLines(t, m2, mt2.ID)
	if last := replies[len(replies)-1]; last.Text != LeavingText || last.Reply == nil || last.Reply.Delivered || last.Reply.Undelivered == "" {
		t.Fatalf("an unconfirmed goodbye must be recorded as not delivered: %+v", last)
	}
}

func TestSpeechIsPrewarmedWhenAVoiceMeetingGoesLive(t *testing.T) {
	q := &timedQuorum{fakeQuorum: newFakeQuorum(), confirm: true}
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{
		Store: st, Quorum: q, BotName: "hive (notes)", ConsentText: testConsent, VoiceOnAddress: true,
		ReconnectMin: 5 * time.Millisecond, ReconnectMax: 20 * time.Millisecond,
		FixedSpokenLines: func(Meeting) []string { return []string{"Hi, I'm Betty.", LeavingText, ""} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	liveMeeting(t, m, q.fakeQuorum, true)
	eventually(t, "prewarm", func() bool {
		q.tmu.Lock()
		defer q.tmu.Unlock()
		return len(q.prewarmed) == 1
	})
	q.tmu.Lock()
	got := q.prewarmed[0]
	q.tmu.Unlock()
	if len(got) != 3 || got[0] != SpeechText(testConsent) || got[1] != "Hi, I'm Betty." || got[2] != LeavingText {
		t.Fatalf("prewarmed %q: want the consent notice first, then the lines as they are spoken, blanks dropped", got)
	}
}

// The companion window polls only while the meeting is live, so the lines
// Quorum still held when hive left are served after the end, once.
func TestCompanionRepliesServeTheLastLinesAfterALeave(t *testing.T) {
	q := newCompanionFake()
	m := newTestManager(t, q, time.Minute)
	ctx := context.Background()
	mt, err := m.StartCompanion(ctx, validStart())
	if err != nil {
		t.Fatal(err)
	}
	q.stream("qc1") <- Event{Type: EventState, MeetingID: "qc1", State: StateLive}
	waitConsent(t, m, mt.ID)
	q.cmu.Lock()
	q.lines = []CompanionLine{{Seq: 1, Text: "notice"}, {Seq: 2, Text: "an answer"}, {Seq: 3, Text: LeavingText}}
	q.cmu.Unlock()
	if _, err := m.Leave(ctx, mt.ID, ReasonHiveLeave); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompanionReplies(ctx, mt.ID, "bob", false, 0); !errors.Is(err, ErrCompanionNotYours) {
		t.Fatalf("another user must not read the last lines: %v", err)
	}
	r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 2)
	if err != nil || r.State != StateEnded || len(r.Lines) != 1 || r.Lines[0].Text != LeavingText || r.NextSeq != 3 {
		t.Fatalf("last lines = %+v, %v", r, err)
	}
	// The cursor passed them: they are forgotten.
	if r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 3); err != nil || len(r.Lines) != 0 || r.NextSeq != 3 {
		t.Fatalf("after fetch = %+v, %v", r, err)
	}
	if r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 0); err != nil || r.Lines == nil || len(r.Lines) != 0 {
		t.Fatalf("fetched lines must not come back: %+v, %v", r, err)
	}
}

func TestCompanionLastLinesExpire(t *testing.T) {
	q := newCompanionFake()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{
		Store: st, Quorum: q, BotName: "hive (notes)", ConsentText: testConsent,
		ReconnectMin: 5 * time.Millisecond, ReconnectMax: 20 * time.Millisecond,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()
	mt, err := m.StartCompanion(ctx, validStart())
	if err != nil {
		t.Fatal(err)
	}
	q.stream("qc1") <- Event{Type: EventState, MeetingID: "qc1", State: StateLive}
	waitConsent(t, m, mt.ID)
	q.cmu.Lock()
	q.lines = []CompanionLine{{Seq: 1, Text: LeavingText}}
	q.cmu.Unlock()
	if _, err := m.EndCompanion(ctx, mt.ID, "ana", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(companionTailGrace)
	mu.Unlock()
	if r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 0); err != nil || len(r.Lines) != 0 {
		t.Fatalf("lines past the grace period = %+v, %v", r, err)
	}
}
