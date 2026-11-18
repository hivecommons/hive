package meetings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

const testConsent = "Hive has joined to take notes and help with research. It records this meeting's transcript. Type 'hive leave' to remove it."

// fakeQuorum implements Quorum in memory. Each Events call drains the
// meeting's channel; closing a stream ends one connection.
type fakeQuorum struct {
	mu        sync.Mutex
	joinErr   error
	joinRes   JoinResult
	leaveErr  error
	chatErr   error
	sayErr    error
	streams   map[string]chan Event
	gone      map[string]bool
	posts     []string
	says      []string
	leaves    []string
	connects  int
	afters    []int64
	joins     []JoinArgs
	deletes   []string
	deleteErr error
}

func newFakeQuorum() *fakeQuorum {
	return &fakeQuorum{
		joinRes: JoinResult{MeetingID: "q1", Platform: PlatformGoogleMeet, State: StateJoining},
		streams: map[string]chan Event{}, gone: map[string]bool{},
	}
}

func (f *fakeQuorum) stream(id string) chan Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.streams[id]
	if !ok {
		ch = make(chan Event, 64)
		f.streams[id] = ch
	}
	return ch
}

func (f *fakeQuorum) Join(_ context.Context, a JoinArgs) (JoinResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joins = append(f.joins, a)
	return f.joinRes, f.joinErr
}

func (f *fakeQuorum) Leave(_ context.Context, id string) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaves = append(f.leaves, id)
	return StateEnded, f.leaveErr
}

func (f *fakeQuorum) ChatPost(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chatErr != nil {
		err := f.chatErr
		f.chatErr = nil
		return err
	}
	f.posts = append(f.posts, text)
	return nil
}

func (f *fakeQuorum) Say(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.says = append(f.says, text)
	return f.sayErr
}

func (f *fakeQuorum) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	return f.deleteErr
}

func (f *fakeQuorum) Events(ctx context.Context, id string, afterSeq int64, fn func(Event) bool) error {
	f.mu.Lock()
	f.connects++
	f.afters = append(f.afters, afterSeq)
	gone := f.gone[id]
	f.mu.Unlock()
	if gone {
		return ErrStreamGone
	}
	ch := f.stream(id)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				f.mu.Lock()
				f.streams[id] = make(chan Event, 64)
				f.mu.Unlock()
				return errors.New("stream ended")
			}
			if !fn(ev) {
				return nil
			}
		}
	}
}

func (f *fakeQuorum) snapshot() (posts, leaves []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...), append([]string(nil), f.leaves...)
}

func newTestManager(t *testing.T, q Quorum, lobby time.Duration) *Manager {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{
		Store: st, Quorum: q, BotName: "hive (notes)", ConsentText: testConsent,
		LobbyTimeout: lobby, ReconnectMin: 5 * time.Millisecond, ReconnectMax: 20 * time.Millisecond,
		ConsentRetry: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	testutil.Eventually(t, 5*time.Second, cond, "timed out waiting for %s", what)
}

func stateIs(m *Manager, id string, st State) func() bool {
	return func() bool {
		mt, err := m.Get(id)
		return err == nil && mt.State == st
	}
}

func TestNewManagerRequiresDeps(t *testing.T) {
	if _, err := NewManager(Options{}); err == nil {
		t.Fatal("want an error")
	}
}

func TestJoinValidation(t *testing.T) {
	m := newTestManager(t, newFakeQuorum(), 0)
	bad := []JoinRequest{
		{},
		{URL: "http://meet.google.com/x"},
		{URL: "not a url"},
		{URL: "https://meet.google.com/x", Platform: "webex"},
		{Platform: "discord", GuildID: "g"},
		{ChannelID: "c"},
	}
	for _, r := range bad {
		if _, err := m.Join(context.Background(), r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Join(%+v) = %v, want ErrInvalidRequest", r, err)
		}
	}
}

func TestLifecycleConsentTranscriptAndHiveLeave(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, time.Minute)
	var mu sync.Mutex
	var updates []Update
	unsub := m.Subscribe(func(u Update) { mu.Lock(); updates = append(updates, u); mu.Unlock() })
	defer unsub()

	mt, err := m.Join(context.Background(), JoinRequest{URL: " https://meet.google.com/abc-defg-hij ", Voice: true, InvitedBy: "owner", Title: "standup"})
	if err != nil {
		t.Fatal(err)
	}
	if mt.NativeID != "q1" || mt.State != StateJoining || !mt.VoiceEnabled || mt.InvitedBy != "owner" || mt.Platform != PlatformGoogleMeet {
		t.Fatalf("joined %+v", mt)
	}
	if err := m.PostChat(context.Background(), mt.ID, "x"); !errors.Is(err, ErrNotLive) {
		t.Fatalf("PostChat before live = %v", err)
	}

	ch := q.stream("q1")
	ch <- Event{Type: EventState, MeetingID: "q1", State: StateLobby}
	eventually(t, "lobby", stateIs(m, mt.ID, StateLobby))
	ch <- Event{Type: EventState, MeetingID: "q1", State: StateLive}
	eventually(t, "consent", func() bool {
		got, _ := m.Get(mt.ID)
		return got.ConsentPostedAt != nil
	})
	if posts, _ := q.snapshot(); len(posts) != 1 || posts[0] != testConsent {
		t.Fatalf("first post must be the consent notice, got %q", posts)
	}
	got, _ := m.Get(mt.ID)
	if got.LiveAt == nil || got.LobbySince != nil {
		t.Fatalf("live record %+v", got)
	}
	if err := m.PostChat(context.Background(), mt.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Say(context.Background(), mt.ID, "hi"); err != nil {
		t.Fatal(err)
	}

	// Echoes of hive's own posts, including the consent text that says
	// "hive leave", never trigger anything.
	ch <- Event{Type: EventChat, MeetingID: "q1", Author: &Person{Name: "Someone"}, Text: testConsent}
	ch <- Event{Type: EventChat, MeetingID: "q1", Author: &Person{Name: "hive (notes)"}, Text: "hive leave"}
	ch <- Event{Type: EventSegment, MeetingID: "q1", Seq: 1, Speaker: &Person{Name: "Ana"}, Text: "partial", Final: false}
	ch <- Event{Type: EventSegment, MeetingID: "q1", Seq: 2, Speaker: &Person{Name: "Ana"}, Text: "we ship friday", Final: true}
	ch <- Event{Type: EventChat, MeetingID: "q1", Seq: 3, Author: &Person{Name: "Bo"}, Text: "agreed"}
	ch <- Event{Type: "mystery", MeetingID: "q1"}
	ch <- Event{Type: EventState, MeetingID: "q1", State: "weird"}
	eventually(t, "transcript", func() bool {
		tr, _ := heardLines(m, mt.ID)
		return len(tr) == 2
	})
	if got, _ := m.Get(mt.ID); got.State != StateLive {
		t.Fatalf("echoes must not end the meeting: %s", got.State)
	}

	ch <- Event{Type: EventChat, MeetingID: "q1", Seq: 4, Author: &Person{Name: "Bo"}, Text: "Hive, please leave"}
	eventually(t, "hive leave", stateIs(m, mt.ID, StateEnded))
	got, _ = m.Get(mt.ID)
	if got.StateReason != ReasonHiveLeave || got.EndedAt == nil {
		t.Fatalf("ended %+v", got)
	}
	if _, leaves := q.snapshot(); len(leaves) != 1 {
		t.Fatalf("leave calls %v", leaves)
	}
	tr, _ := heardLines(m, mt.ID)
	if len(tr) != 3 || tr[0].Text != "we ship friday" {
		t.Fatalf("transcript %+v", tr)
	}
	mu.Lock()
	n := len(updates)
	sawPartial := false
	for _, u := range updates {
		if u.Event != nil && u.Event.Text == "partial" {
			sawPartial = true
		}
	}
	mu.Unlock()
	if n == 0 || !sawPartial {
		t.Fatalf("listeners saw %d updates, partial=%v", n, sawPartial)
	}
	// Leaving an ended meeting is a no-op.
	if again, err := m.Leave(context.Background(), mt.ID, ReasonOwnerLeave); err != nil || again.StateReason != ReasonHiveLeave {
		t.Fatalf("Leave on ended = %+v, %v", again, err)
	}
	list, _ := m.List()
	if len(list) != 1 {
		t.Fatalf("list %d", len(list))
	}
}

func TestConsentRetriedAfterFailure(t *testing.T) {
	q := newFakeQuorum()
	q.chatErr = errors.New("rate_limited")
	m := newTestManager(t, q, 0)
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://teams.microsoft.com/l/x"})
	if err != nil {
		t.Fatal(err)
	}
	ch := q.stream("q1")
	ch <- Event{Type: EventState, State: StateLive}
	eventually(t, "live", stateIs(m, mt.ID, StateLive))
	if err := m.PostChat(context.Background(), mt.ID, "x"); !errors.Is(err, ErrConsentPending) {
		t.Fatalf("PostChat before consent = %v", err)
	}
	if _, err := m.Say(context.Background(), mt.ID, "x"); !errors.Is(err, ErrConsentPending) {
		t.Fatalf("Say before consent = %v", err)
	}
	ch <- Event{Type: EventSegment, Speaker: &Person{Name: "Ana"}, Text: "hello", Final: true}
	eventually(t, "consent retry", func() bool {
		got, _ := m.Get(mt.ID)
		return got.ConsentPostedAt != nil
	})
}

func TestOwnerLeaveAndVoice(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	mt, err := m.Join(context.Background(), JoinRequest{Platform: "discord", GuildID: "g", ChannelID: "c", URL: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if mt.URL != "" || mt.GuildID != "g" || mt.ChannelID != "c" {
		t.Fatalf("discord join %+v", mt)
	}
	v, err := m.SetVoice(mt.ID, true)
	if err != nil || !v.VoiceEnabled {
		t.Fatalf("SetVoice = %+v, %v", v, err)
	}
	q.leaveErr = &ToolError{Code: CodeNotFound}
	got, err := m.Leave(context.Background(), mt.ID, ReasonOwnerLeave)
	if err != nil || got.State != StateEnded || got.StateReason != ReasonOwnerLeave {
		t.Fatalf("Leave = %+v, %v", got, err)
	}
	if _, err := m.SetVoice("mtg-missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetVoice missing = %v", err)
	}
	if _, err := m.Leave(context.Background(), "mtg-missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Leave missing = %v", err)
	}
	if err := m.PostChat(context.Background(), "mtg-missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PostChat missing = %v", err)
	}
	if _, err := m.Say(context.Background(), "mtg-missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Say missing = %v", err)
	}
	if _, err := m.Say(context.Background(), mt.ID, "x"); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Say ended = %v", err)
	}
}

func TestLeaveErrorKeepsMeeting(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	mt, _ := m.Join(context.Background(), JoinRequest{URL: "https://zoom.us/j/1"})
	q.leaveErr = &ToolError{Code: CodeUpstreamError}
	if _, err := m.Leave(context.Background(), mt.ID, ReasonOwnerLeave); err == nil {
		t.Fatal("want the upstream error")
	}
	if got, _ := m.Get(mt.ID); got.State.Terminal() {
		t.Fatal("a failed leave must not end the record")
	}
}

func TestJoinFailures(t *testing.T) {
	q := newFakeQuorum()
	q.joinErr = &ToolError{Tool: "meeting.join", Code: CodeBlocked, Message: "guests refused"}
	m := newTestManager(t, q, 0)
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://zoom.us/j/1"})
	if !IsCode(err, CodeBlocked) || mt.State != StateBlocked || mt.EndedAt == nil {
		t.Fatalf("blocked join = %+v, %v", mt, err)
	}
	q.joinErr = errors.New("down")
	mt, err = m.Join(context.Background(), JoinRequest{URL: "https://zoom.us/j/2"})
	if err == nil || mt.State != StateFailed {
		t.Fatalf("failed join = %+v, %v", mt, err)
	}
	q.joinErr = nil
	q.joinRes = JoinResult{MeetingID: "q9", State: "odd"}
	mt, err = m.Join(context.Background(), JoinRequest{URL: "https://zoom.us/j/3"})
	if err != nil || mt.State != StateJoining || mt.Platform != "" {
		t.Fatalf("odd state join = %+v, %v", mt, err)
	}
}

func TestQuorumEndsMeeting(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	mt, _ := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	q.stream("q1") <- Event{Type: EventState, State: StateEnded, Reason: "host ended"}
	eventually(t, "ended", stateIs(m, mt.ID, StateEnded))
	if got, _ := m.Get(mt.ID); got.StateReason != "host ended" {
		t.Fatalf("reason %q", got.StateReason)
	}
}

func TestReconnectAndStreamGone(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	mt, _ := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	close(q.stream("q1"))
	eventually(t, "reconnect", func() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.connects >= 2 })
	q.mu.Lock()
	q.gone["q1"] = true
	ch := q.streams["q1"]
	q.mu.Unlock()
	close(ch)
	eventually(t, "lost", stateIs(m, mt.ID, StateFailed))
	if got, _ := m.Get(mt.ID); got.StateReason != ReasonQuorumLost {
		t.Fatalf("reason %q", got.StateReason)
	}
}

func TestLobbyTimeout(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 30*time.Millisecond)
	mt, _ := m.Join(context.Background(), JoinRequest{URL: "https://teams.microsoft.com/l/x"})
	q.stream("q1") <- Event{Type: EventState, State: StateLobby}
	eventually(t, "lobby timeout", stateIs(m, mt.ID, StateFailed))
	got, _ := m.Get(mt.ID)
	if got.StateReason != CodeLobbyTimeout {
		t.Fatalf("reason %q", got.StateReason)
	}
	if _, leaves := q.snapshot(); len(leaves) != 1 {
		t.Fatalf("leaves %v", leaves)
	}
	// An expiry for a meeting no longer in the lobby does nothing.
	m.lobbyExpired(mt.ID)
	m.lobbyExpired("mtg-missing")
}

func TestLobbyClearedWhenAdmitted(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, time.Hour)
	mt, _ := m.Join(context.Background(), JoinRequest{URL: "https://teams.microsoft.com/l/x"})
	ch := q.stream("q1")
	ch <- Event{Type: EventState, State: StateLobby}
	eventually(t, "lobby timer", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.lobby) == 1 })
	ch <- Event{Type: EventState, State: StateLive}
	eventually(t, "timer cleared", func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.lobby) == 0 })
	_ = mt
}

func TestResume(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(dir)
	now := time.Now()
	past := now.Add(-time.Hour)
	_ = st.Save(Meeting{ID: "mtg-live", NativeID: "q-live", State: StateLive, CreatedAt: now})
	_ = st.Save(Meeting{ID: "mtg-lobby", NativeID: "q-lobby", State: StateLobby, LobbySince: &past, CreatedAt: now})
	_ = st.Save(Meeting{ID: "mtg-done", NativeID: "q-done", State: StateEnded, CreatedAt: now})
	_ = st.Save(Meeting{ID: "mtg-new", State: StateRequested, CreatedAt: now})
	q := newFakeQuorum()
	m, err := NewManager(Options{Store: st, Quorum: q, ConsentText: testConsent, LobbyTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if n := m.Resume(); n != 2 {
		t.Fatalf("resumed %d", n)
	}
	eventually(t, "overdue lobby left", stateIs(m, "mtg-lobby", StateFailed))
	q.stream("q-live") <- Event{Type: EventChat, Author: &Person{Name: "Ana"}, Text: "hi"}
	eventually(t, "consent after resume", func() bool {
		got, _ := m.Get("mtg-live")
		return got.ConsentPostedAt != nil
	})

	// A store that cannot be listed resumes nothing.
	_ = os.RemoveAll(dir)
	if n := m.Resume(); n != 0 {
		t.Fatalf("resumed %d from a missing store", n)
	}
}

func TestIsLeaveCommand(t *testing.T) {
	for _, s := range []string{"hive leave", "Hive, leave", "HIVE: please leave", "ok hive leave now"} {
		if !IsLeaveCommand(s) {
			t.Errorf("%q should match", s)
		}
	}
	for _, s := range []string{"leave hive out of it", "archive leave policy", "hive is great", "we should leave"} {
		if IsLeaveCommand(s) {
			t.Errorf("%q should not match", s)
		}
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Dir() != dir {
		t.Fatal("Dir")
	}
	if err := st.Save(Meeting{ID: "../evil"}); err == nil {
		t.Fatal("want an invalid id error")
	}
	if _, err := st.Get("../evil"); err == nil {
		t.Fatal("want an invalid id error")
	}
	if err := st.AppendTranscript("../evil", Event{}); err == nil {
		t.Fatal("want an invalid id error")
	}
	if _, err := st.Transcript("../evil"); err == nil {
		t.Fatal("want an invalid id error")
	}
	if err := st.PurgeTranscript("../evil"); err == nil {
		t.Fatal("want an invalid id error")
	}
	if _, err := st.Get("mtg-none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if tr, err := st.Transcript("mtg-none"); err != nil || tr != nil {
		t.Fatalf("Transcript missing = %v, %v", tr, err)
	}
	older := Meeting{ID: "mtg-a", CreatedAt: time.Unix(1, 0)}
	newer := Meeting{ID: "mtg-b", CreatedAt: time.Unix(2, 0)}
	_ = st.Save(older)
	_ = st.Save(newer)
	_ = os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "mtg-broken"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "mtg-broken", meetingFile), []byte("{"), 0o600)
	list, err := st.List()
	if err != nil || len(list) != 2 || list[0].ID != "mtg-b" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if _, err := st.Get("mtg-broken"); err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("broken = %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "mtg-a", meetingFile)); fi.Mode().Perm() != storeFileMode {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	// Events without raw bytes are marshalled; garbage lines are skipped.
	if err := st.AppendTranscript("mtg-a", Event{Type: EventChat, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(dir, "mtg-a", transcriptFile), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("garbage\n")
	_ = f.Close()
	tr, err := st.Transcript("mtg-a")
	if err != nil || len(tr) != 1 || tr[0].Text != "hi" {
		t.Fatalf("Transcript = %+v, %v", tr, err)
	}
	if err := st.PurgeTranscript("mtg-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.PurgeTranscript("mtg-a"); err != nil {
		t.Fatal(err)
	}
	// A directory in place of the transcript cannot be opened for append.
	_ = os.MkdirAll(filepath.Join(dir, "mtg-a", transcriptFile, "x"), 0o700)
	if err := st.AppendTranscript("mtg-a", Event{}); err == nil {
		t.Fatal("want an open error")
	}
	if err := st.PurgeTranscript("mtg-a"); err == nil {
		t.Fatal("want a remove error for a non-empty dir")
	}
	if _, err := st.Transcript("mtg-a"); err == nil {
		t.Fatal("want a read error for a directory")
	}
	// A record that cannot be written into a file-blocked directory.
	_ = os.WriteFile(filepath.Join(dir, "mtg-c"), []byte("x"), 0o600)
	if err := st.Save(Meeting{ID: "mtg-c"}); err == nil {
		t.Fatal("want a mkdir error")
	}
	if _, err := NewStore(filepath.Join(dir, "stray", "sub")); err == nil {
		t.Fatal("want a store dir error")
	}
	// A missing root lists as an error.
	_ = os.RemoveAll(dir)
	if _, err := st.List(); err == nil {
		t.Fatal("want a list error")
	}
	if err := writeAtomic(filepath.Join(dir, "nope", "f"), nil); err == nil {
		t.Fatal("want a temp file error")
	}
}

func TestStateTerminal(t *testing.T) {
	for _, s := range []State{StateEnded, StateFailed, StateBlocked, StateHandedOff, StatePurged} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []State{StateRequested, StateJoining, StateLobby, StateLive} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
	if !strings.HasPrefix(newMeetingID(), "mtg-") {
		t.Fatal("id prefix")
	}
	if _, err := parseEvent([]byte("{")); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestStoreDocs(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	type doc struct{ A int }
	if err := st.SaveDoc("mtg-a", "Bad Name", doc{}); err == nil {
		t.Fatal("want a name error")
	}
	if err := st.LoadDoc("mtg-a", "../x.json", &doc{}); err == nil {
		t.Fatal("want a name error")
	}
	if err := st.SaveDoc("../x", "session.json", doc{}); err == nil {
		t.Fatal("want an id error")
	}
	if err := st.LoadDoc("../x", "session.json", &doc{}); err == nil {
		t.Fatal("want an id error")
	}
	if err := st.SaveDoc("mtg-a", "session.json", func() {}); err == nil {
		t.Fatal("want a marshal error")
	}
	var d doc
	if err := st.LoadDoc("mtg-a", "session.json", &d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if err := st.SaveDoc("mtg-a", "session.json", doc{A: 3}); err != nil {
		t.Fatal(err)
	}
	if err := st.LoadDoc("mtg-a", "session.json", &d); err != nil || d.A != 3 {
		t.Fatalf("LoadDoc = %+v, %v", d, err)
	}
	_ = os.MkdirAll(filepath.Join(st.Dir(), "mtg-a", "notes.json"), 0o700)
	if err := st.LoadDoc("mtg-a", "notes.json", &d); err == nil {
		t.Fatal("want a read error")
	}
	_ = os.WriteFile(filepath.Join(st.Dir(), "mtg-f"), []byte("x"), 0o600)
	if err := st.SaveDoc("mtg-f", "session.json", doc{}); err == nil {
		t.Fatal("want a mkdir error")
	}
}

func TestLeaveMatcherPhrases(t *testing.T) {
	l := NewLeaveMatcher([]string{"Betty leave", "hive leave", "  "})
	for _, s := range []string{"betty leave", "Betty, please leave", "BETTY: leave now", "ok hive leave"} {
		if !l.Match(s) {
			t.Errorf("%q should match", s)
		}
	}
	for _, s := range []string{"leave betty out of it", "betty is leaving soon", "we should leave"} {
		if l.Match(s) {
			t.Errorf("%q should not match", s)
		}
	}
	if d := NewLeaveMatcher(nil); !d.Match("hive leave") || d.Match("betty leave") {
		t.Fatal("no phrases means the default hive leave")
	}
}

func TestIsBotName(t *testing.T) {
	for _, c := range []struct {
		name, bot string
		want      bool
	}{
		{"Betty Bee from the hive", "Betty Bee from the hive", true},
		{" betty bee FROM the hive ", "Betty Bee from the hive", true},
		{"Betty Bee from the hive (Guest)", "Betty Bee from the hive", true},
		{"Betty", "Betty Bee from the hive", false},
		{"(Guest)", "Betty Bee from the hive", false},
		{"anyone", "", false},
	} {
		if got := IsBotName(c.name, c.bot); got != c.want {
			t.Errorf("IsBotName(%q, %q) = %v", c.name, c.bot, got)
		}
	}
}

func TestPersonaLeavePhraseAndDecoratedEcho(t *testing.T) {
	q := newFakeQuorum()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Options{
		Store: st, Quorum: q, BotName: "Betty Bee from the hive", ConsentText: "Betty has joined. Type 'betty leave' to remove Betty.",
		LeavePhrases: []string{"betty leave", "hive leave"}, ReconnectMin: 5 * time.Millisecond, ConsentRetry: time.Millisecond,
		VoiceID: " af_heart ",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/abc-defg-hij"})
	if err != nil {
		t.Fatal(err)
	}
	// meeting.join carries the persona's voice (quorum v0.2.0 voice_id).
	q.mu.Lock()
	if len(q.joins) != 1 || q.joins[0].VoiceID != "af_heart" {
		t.Fatalf("joins %+v", q.joins)
	}
	q.mu.Unlock()
	ch := q.stream("q1")
	ch <- Event{Type: EventState, MeetingID: "q1", State: StateLive}
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	// The bot's own lines, under the platform's decorated name or as a
	// speaker, never trigger the leave.
	ch <- Event{Type: EventChat, MeetingID: "q1", Seq: 1, Author: &Person{Name: "Betty Bee from the hive (Guest)"}, Text: "say 'betty leave' to remove me"}
	ch <- Event{Type: EventSegment, MeetingID: "q1", Seq: 2, Speaker: &Person{Name: "Betty Bee from the hive"}, Text: "betty leave", Final: true}
	ch <- Event{Type: EventChat, MeetingID: "q1", Seq: 3, Author: &Person{Name: "Ana"}, Text: "hello"}
	eventually(t, "transcript", func() bool { tr, _ := heardLines(m, mt.ID); return len(tr) == 1 })
	ch <- Event{Type: EventChat, MeetingID: "q1", Seq: 4, Author: &Person{Name: "Ana"}, Text: "Betty, please leave"}
	eventually(t, "betty leave", stateIs(m, mt.ID, StateEnded))
	if got, _ := m.Get(mt.ID); got.StateReason != ReasonHiveLeave {
		t.Fatalf("reason %q", got.StateReason)
	}
}
