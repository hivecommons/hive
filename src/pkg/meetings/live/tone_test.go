package live

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
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
	testutil.Eventually(tb, 5*time.Second, func() bool { return t.count() == want }, "%d tone(s), have %d", want, t.count())
}

// refused asserts the tone is not allowed now, and why.
func refused(t *testing.T, s *session, asker, why string) {
	t.Helper()
	if ok, got := s.toneAllowed(asker); ok || got != why {
		t.Fatalf("toneAllowed(%q) = %v, %q; want refused: %q", asker, ok, got, why)
	}
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
// off, the tone is refused and nothing is played.
func TestToneNotPlayedWhenVoiceOff(t *testing.T) {
	tm, s, _ := toneSession(t)
	m := tm.m[s.id]
	m.VoiceEnabled = false
	tm.m[s.id] = m
	refused(t, s, "Bo", speakVoiceDisabled)
	s.onPersonaReply(context.Background(), toneReply)

	tm2, s2, _ := toneSession(t)
	s2.r.opts.Policy.VoiceOnAddress = false
	refused(t, s2, "Bo", speakPolicyOff)
	s2.onPersonaReply(context.Background(), toneReply)

	// A tone that is allowed goes out; if either refused one had slipped
	// through it would already have been counted by the time this lands.
	tm3, s3, _ := toneSession(t)
	s3.onPersonaReply(context.Background(), toneReply)
	tm3.eventually(t, 1)
	if tm.count() != 0 || tm2.count() != 0 {
		t.Fatalf("no tone with voice off: %d, %d", tm.count(), tm2.count())
	}
}

// A companion meeting has no voice in the call, yet its chime plays: the
// desktop companion plays it on the host's computer. voice_on_address off
// still refuses it.
func TestToneOnCompanionIgnoresMeetingVoice(t *testing.T) {
	tm, s, _ := toneSession(t)
	m := tm.m[s.id]
	m.Platform, m.VoiceEnabled = meetings.PlatformCompanion, false
	tm.m[s.id] = m
	if ok, why := s.toneAllowed("Bo"); !ok {
		t.Fatalf("companion tone refused: %q", why)
	}
	s.onPersonaReply(context.Background(), toneReply)
	tm.eventually(t, 1)

	s.r.opts.Policy.VoiceOnAddress = false
	refused(t, s, "Bo", speakPolicyOff)
}

// She does not chime over her own speech or into someone else's sentence;
// the asker's own partial does not stop it.
func TestToneNotOverSpeechOrCrosstalk(t *testing.T) {
	tm, s, clk := toneSession(t)
	ctx := context.Background()
	s.queueVoice(betty.Reply{}, strings.Repeat("x", 60))
	s.flushVoice(ctx)
	refused(t, s, "Bo", "speech queued or playing")
	s.onPersonaReply(ctx, toneReply)

	clk.add(time.Minute) // the speech has played out
	s.notePartial("Cy")
	refused(t, s, "Bo", "someone else is speaking")
	s.onPersonaReply(ctx, toneReply)
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
