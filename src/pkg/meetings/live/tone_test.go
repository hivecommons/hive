package live

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// tonerMeetings is a Meetings that can also play the tone and counts it.
type tonerMeetings struct {
	*fakeMeetings
	tmu   sync.Mutex
	tones int
	err   error
}

func (t *tonerMeetings) Tone(context.Context, string) error {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	t.tones++
	return t.err
}

func (t *tonerMeetings) count() int {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	return t.tones
}

func (t *tonerMeetings) eventually(tb testing.TB, want int) {
	tb.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if t.count() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	tb.Fatalf("tones %d, want %d", t.count(), want)
}

// toneSession is a voice-on session whose Meetings plays tones.
func toneSession(t *testing.T) (*tonerMeetings, *session, *stepClock) {
	t.Helper()
	h, s, clk := voiceSession(t, Policy{ChatPostsPerMinute: 6, VoiceOnAddress: true})
	tm := &tonerMeetings{fakeMeetings: h.mt}
	s.r.opts.Meetings = tm
	return tm, s, clk
}

var toneReply = betty.Reply{Kind: betty.KindVoice, Trigger: betty.TriggerTone, Asker: "Bo", VoiceWanted: true}

// The tone plays when voice is on, and is not a line: nothing is said or
// posted for it.
func TestToneReplyPlaysTone(t *testing.T) {
	tm, s, _ := toneSession(t)
	s.onPersonaReply(context.Background(), toneReply)
	tm.eventually(t, 1)
	if posts, says := tm.got(); len(posts) != 0 || len(says) != 0 {
		t.Fatalf("a tone is not a line: posts %q says %q", posts, says)
	}
}

// With the meeting's voice off (chat-only fallback), or voice_on_address
// off, nothing plays.
func TestToneNotPlayedWhenVoiceOff(t *testing.T) {
	tm, s, _ := toneSession(t)
	m := tm.m[s.id]
	m.VoiceEnabled = false
	tm.m[s.id] = m
	s.onPersonaReply(context.Background(), toneReply)

	tm2, s2, _ := toneSession(t)
	s2.r.opts.Policy.VoiceOnAddress = false
	s2.onPersonaReply(context.Background(), toneReply)

	time.Sleep(100 * time.Millisecond)
	if tm.count() != 0 || tm2.count() != 0 {
		t.Fatalf("no tone with voice off: %d, %d", tm.count(), tm2.count())
	}
}

// She does not chime over her own speech or into someone else's sentence;
// the asker's own partial does not stop it.
func TestToneNotOverSpeechOrCrosstalk(t *testing.T) {
	tm, s, clk := toneSession(t)
	ctx := context.Background()
	s.queueVoice(betty.Reply{}, strings.Repeat("x", 60))
	s.flushVoice(ctx)
	s.onPersonaReply(ctx, toneReply)

	clk.add(time.Minute) // the speech has played out
	s.notePartial("Cy")
	s.onPersonaReply(ctx, toneReply)

	time.Sleep(100 * time.Millisecond)
	if tm.count() != 0 {
		t.Fatalf("no tone over speech or crosstalk, got %d", tm.count())
	}

	s.notePartial("Bo") // the asker's own partial
	s.onPersonaReply(ctx, toneReply)
	tm.eventually(t, 1)

	s.notePartial("Cy")
	clk.add(toneCrosstalk + time.Millisecond) // Cy stopped a moment ago
	s.onPersonaReply(ctx, toneReply)
	tm.eventually(t, 2)
}

// A failing audio path is dropped silently: nothing is recorded, nothing
// falls back to chat.
func TestToneFailureIsSilent(t *testing.T) {
	tm, s, _ := toneSession(t)
	tm.err = meetings.ErrNotLive
	s.onPersonaReply(context.Background(), toneReply)
	tm.eventually(t, 1)
	if posts, says := tm.got(); len(posts) != 0 || len(says) != 0 {
		t.Fatalf("a failed tone says and posts nothing: %q %q", posts, says)
	}
	if s.state.Dropped != 0 {
		t.Fatalf("a failed tone is not a drop: %d", s.state.Dropped)
	}
}

// A Meetings with no tone support plays nothing and does not fail.
func TestToneWithoutSupportIsNoOp(t *testing.T) {
	_, s, _ := voiceSession(t, Policy{ChatPostsPerMinute: 6, VoiceOnAddress: true})
	s.onPersonaReply(context.Background(), toneReply)
}
