package meetings

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// confirmingQuorum is a fakeQuorum whose chat.post can confirm delivery
// (quorum 0.2.2 chat.post confirm:true).
type confirmingQuorum struct {
	*fakeQuorum
	cmu       sync.Mutex
	confirm   []bool  // answers in order; the last one repeats
	confirmEr []error // errors in order; the last one repeats
	tries     int
}

func (c *confirmingQuorum) ChatPostConfirmed(_ context.Context, _ string, text string) (bool, error) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	i := c.tries
	c.tries++
	pick := func(n int) int { return min(i, n-1) }
	var err error
	if len(c.confirmEr) > 0 {
		err = c.confirmEr[pick(len(c.confirmEr))]
	}
	if err != nil {
		return false, err
	}
	ok := true
	if len(c.confirm) > 0 {
		ok = c.confirm[pick(len(c.confirm))]
	}
	if ok {
		c.fakeQuorum.mu.Lock()
		c.fakeQuorum.posts = append(c.fakeQuorum.posts, text)
		c.fakeQuorum.mu.Unlock()
	}
	return ok, nil
}

func (c *confirmingQuorum) says() []string {
	c.fakeQuorum.mu.Lock()
	defer c.fakeQuorum.mu.Unlock()
	return append([]string(nil), c.fakeQuorum.says...)
}

var errNoComposer = &ToolError{Tool: "chat.post", Code: CodePlatformUnavailable, Message: "the chat line was not posted: no_composer"}

// liveTeams joins a Teams meeting (voice as given), runs before (if any)
// while it is still joining, and takes it live.
func liveTeams(t *testing.T, q *confirmingQuorum, voice bool, before ...func(*Manager, Meeting)) (*Manager, Meeting) {
	t.Helper()
	m := newTestManager(t, q, 0)
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://teams.microsoft.com/l/x", Voice: voice})
	if err != nil {
		t.Fatal(err)
	}
	if mt.VoiceCapable != voice || mt.VoiceEnabled != voice {
		t.Fatalf("joined %+v", mt)
	}
	for _, fn := range before {
		fn(m, mt)
	}
	q.stream("q1") <- Event{Type: EventState, State: StateLive}
	return m, mt
}

func TestConsentConfirmedInChat(t *testing.T) {
	q := &confirmingQuorum{fakeQuorum: newFakeQuorum()}
	m, mt := liveTeams(t, q, true)
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	got, _ := m.Get(mt.ID)
	if got.ConsentVia != ConsentViaChat || got.State != StateLive || got.ConsentError != "" {
		t.Fatalf("meeting %+v", got)
	}
	if s := q.says(); len(s) != 0 {
		t.Fatalf("a confirmed chat notice must not be spoken too: %q", s)
	}
}

func TestConsentSpokenWhenChatFails(t *testing.T) {
	q := &confirmingQuorum{fakeQuorum: newFakeQuorum(), confirmEr: []error{errNoComposer}}
	m, mt := liveTeams(t, q, true)
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	got, _ := m.Get(mt.ID)
	if got.ConsentVia != ConsentViaVoice || got.State != StateLive {
		t.Fatalf("meeting %+v", got)
	}
	if s := q.says(); len(s) != 1 || s[0] != testConsent {
		t.Fatalf("says %q, want the consent notice", s)
	}
	if posts, leaves := q.snapshot(); len(posts) != 0 || len(leaves) != 0 {
		t.Fatalf("posts %q leaves %q", posts, leaves)
	}
	// Once the notice is delivered, replies go through.
	if _, err := m.Say(context.Background(), mt.ID, "hello"); err != nil {
		t.Fatal(err)
	}
}

func TestConsentUnconfirmedChatAndVoiceOffLeaves(t *testing.T) {
	// Unconfirmed (an older Quorum, or no verdict in time) is not posted.
	q := &confirmingQuorum{fakeQuorum: newFakeQuorum(), confirm: []bool{false}}
	m, mt := liveTeams(t, q, false)
	eventually(t, "left", stateIs(m, mt.ID, StateEnded))
	got, _ := m.Get(mt.ID)
	if got.ConsentPostedAt != nil || got.StateReason != ReasonConsentUndelivered {
		t.Fatalf("meeting %+v", got)
	}
	if !strings.Contains(got.ConsentError, "chat delivery not confirmed") || !strings.Contains(got.ConsentError, "joined without voice") {
		t.Fatalf("consent error %q", got.ConsentError)
	}
	if _, leaves := q.snapshot(); len(leaves) != 1 || leaves[0] != "q1" {
		t.Fatalf("leaves %q", leaves)
	}
	if s := q.says(); len(s) != 0 {
		t.Fatalf("voice is off, nothing may be spoken: %q", s)
	}
}

func TestConsentChatAndVoiceFailLeaves(t *testing.T) {
	fq := newFakeQuorum()
	fq.sayErr = &ToolError{Tool: "say", Code: CodePlatformUnavailable, Message: "speak is not available"}
	q := &confirmingQuorum{fakeQuorum: fq, confirmEr: []error{errNoComposer}}
	m, mt := liveTeams(t, q, true)
	eventually(t, "left", stateIs(m, mt.ID, StateEnded))
	got, _ := m.Get(mt.ID)
	if got.ConsentPostedAt != nil || got.StateReason != ReasonConsentUndelivered ||
		!strings.Contains(got.ConsentError, "no_composer") || !strings.Contains(got.ConsentError, "voice failed") {
		t.Fatalf("meeting %+v", got)
	}
}

// The live Teams test: chat had no composer and the owner had turned voice
// replies off, so she left at once. The consent notice is not a voice
// reply: a bot that joined able to speak says it.
func TestConsentSpokenWhenVoiceRepliesOff(t *testing.T) {
	q := &confirmingQuorum{fakeQuorum: newFakeQuorum(), confirmEr: []error{errNoComposer}}
	m, mt := liveTeams(t, q, true, func(m *Manager, mt Meeting) {
		if _, err := m.SetVoice(mt.ID, false); err != nil {
			t.Fatal(err)
		}
	})
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	got, _ := m.Get(mt.ID)
	if got.ConsentVia != ConsentViaVoice || got.State != StateLive || got.VoiceEnabled {
		t.Fatalf("meeting %+v", got)
	}
	if s := q.says(); len(s) != 1 || s[0] != testConsent {
		t.Fatalf("says %q, want the consent notice", s)
	}
}

func TestConsentTransientFailuresBounded(t *testing.T) {
	rl := &ToolError{Tool: "chat.post", Code: CodeRateLimited, Message: "slow down"}
	q := &confirmingQuorum{fakeQuorum: newFakeQuorum(), confirmEr: []error{rl}}
	m, mt := liveTeams(t, q, false)
	ch := q.stream("q1")
	// Each line is a chance to retry; after ConsentAttempts hive gives up.
	go func() {
		for i := 0; i < 20; i++ {
			select {
			case ch <- Event{Type: EventSegment, Speaker: &Person{Name: "Ana"}, Text: "hello", Final: true}:
			case <-time.After(time.Second):
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	eventually(t, "left", stateIs(m, mt.ID, StateEnded))
	q.cmu.Lock()
	tries := q.tries
	q.cmu.Unlock()
	if tries != DefaultConsentAttempts {
		t.Fatalf("consent tried %d times, want %d", tries, DefaultConsentAttempts)
	}
	if got, _ := m.Get(mt.ID); got.StateReason != ReasonConsentUndelivered {
		t.Fatalf("meeting %+v", got)
	}
}

func TestConsentTransientThenConfirmed(t *testing.T) {
	nl := &ToolError{Tool: "chat.post", Code: CodeNotLive, Message: "the bot is not in the meeting"}
	q := &confirmingQuorum{fakeQuorum: newFakeQuorum(), confirmEr: []error{nl, nil}}
	m, mt := liveTeams(t, q, false)
	eventually(t, "consent error", func() bool { got, _ := m.Get(mt.ID); return got.ConsentError != "" })
	q.stream("q1") <- Event{Type: EventSegment, Speaker: &Person{Name: "Ana"}, Text: "hello", Final: true}
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })
	if got, _ := m.Get(mt.ID); got.ConsentVia != ConsentViaChat || got.ConsentError != "" || got.State != StateLive {
		t.Fatalf("meeting %+v", got)
	}
}
