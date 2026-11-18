package live

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *stepClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// voiceSession is a session (not running) on a meeting with voice on and a
// controllable clock, for driving the voice queue directly.
func voiceSession(t *testing.T, p Policy) (*harness, *session, *stepClock) {
	t.Helper()
	clk := &stepClock{now: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	h := newHarness(t, p, func(o *Options) {
		o.Now = clk.Now
		o.Persona = func(meetings.Meeting) *betty.Engine {
			return betty.New(betty.SettingsFrom(config.MeetingsPersonaConfig{}.Resolve()), betty.Deps{})
		}
	})
	h.m.VoiceEnabled = true
	h.mt.m[h.m.ID] = h.m
	return h, h.r.newSession(h.m, h.spek), clk
}

// Answers ready together are spoken shortest first; one that would wait
// behind more than voiceBacklogMax of speech goes to chat, with one spoken
// note.
func TestFlushVoiceShortestFirstAndOverflow(t *testing.T) {
	h, s, _ := voiceSession(t, Policy{ChatPostsPerMinute: 6, VoiceOnAddress: true})
	short := "It ships Friday."
	mid := strings.Repeat("m", 150)  // 10 s
	long := strings.Repeat("l", 200) // about 13 s
	s.queueVoice(betty.Reply{}, long)
	s.queueVoice(betty.Reply{}, short)
	s.queueVoice(betty.Reply{}, mid)
	s.flushVoice(context.Background())
	posts, says := h.mt.got()
	want := []string{short, mid, "I've put the rest in chat."}
	if strings.Join(says, "|") != strings.Join(want, "|") {
		t.Fatalf("says %q, want %q", says, want)
	}
	if len(posts) != 1 || posts[0] != long {
		t.Fatalf("posts %q, want the long answer in chat", posts)
	}
	if len(s.voice.pending) != 0 {
		t.Fatal("the flush leaves nothing pending")
	}
}

// An acknowledgement is skipped behind more than ackMaxBacklog of queued
// speech, and spoken again once it has played.
func TestAckSkippedBehindLongSpeech(t *testing.T) {
	_, s, clk := voiceSession(t, Policy{ChatPostsPerMinute: 6, VoiceOnAddress: true})
	ctx := context.Background()
	s.queueVoice(betty.Reply{}, strings.Repeat("x", 180)) // 12 s
	s.flushVoice(ctx)
	ack := betty.Reply{Kind: betty.KindVoice, Text: "One moment.", Trigger: betty.TriggerAck}
	if s.speakAck(ctx, ack, ack.Text) {
		t.Fatal("an ack behind 12 s of speech is skipped")
	}
	clk.add(10 * time.Second)
	if !s.speakAck(ctx, ack, ack.Text) {
		t.Fatal("an ack behind 2 s of speech is spoken")
	}
}

// Answers to addressed requests may take a small burst past the chat rate;
// Spek's posts may not.
func TestAnswerBurstPastChatRate(t *testing.T) {
	h, s, _ := voiceSession(t, Policy{ChatPostsPerMinute: 1, ChatAnswerBurst: 3})
	ctx := context.Background()
	if !s.post(ctx, "spek one") {
		t.Fatal("first post")
	}
	if s.post(ctx, "spek two") {
		t.Fatal("spek posts stay within the chat rate")
	}
	for i := range 3 {
		if !s.postAnswer(ctx, "answer") {
			t.Fatalf("answer %d within the burst refused", i+1)
		}
	}
	if s.postAnswer(ctx, "answer") {
		t.Fatal("past the burst an answer waits")
	}
	if posts, _ := h.mt.got(); len(posts) != 4 {
		t.Fatalf("posts %q", posts)
	}
	_, none, _ := voiceSession(t, Policy{ChatPostsPerMinute: 1})
	none.post(ctx, "x")
	if none.postAnswer(ctx, "y") {
		t.Fatal("no burst configured: the chat rate holds")
	}
}

func TestSpeechTime(t *testing.T) {
	if d := speechTime("hi"); d != voiceMinClip {
		t.Fatalf("short clip %v", d)
	}
	if d := speechTime(strings.Repeat("a", 150)); d != 10*time.Second {
		t.Fatalf("150 chars %v", d)
	}
	var v voiceQueue
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	v.queued(now, strings.Repeat("a", 30))
	v.queued(now, strings.Repeat("a", 30))
	if b := v.backlog(now); b != 4*time.Second {
		t.Fatalf("two queued clips back to back: %v", b)
	}
	if b := v.backlog(now.Add(time.Minute)); b != 0 {
		t.Fatalf("played out: %v", b)
	}
}

// Answers flushed together each open with the topic they answer: the live
// Discord test posted "Yes, Andy, I can hear you" (to an earlier "Betty,
// are you there?") beside the board pressure answer with no attribution.
func TestBatchedAnswersAreAttributed(t *testing.T) {
	h, s, _ := voiceSession(t, Policy{ChatPostsPerMinute: 6, VoiceOnAddress: true})
	h.m.VoiceEnabled = false
	h.mt.m[h.m.ID] = h.m
	ctx := context.Background()
	hear := "Yes, Andy, I can hear you loud and clear."
	bp := "On board pressure: BP is 40/12."
	s.queueVoice(betty.Reply{Asker: "Andy Anderson", Topic: "are you there"}, hear)
	s.queueVoice(betty.Reply{Asker: "Andy Anderson", Topic: "board pressure", Attributed: true}, bp)
	s.flushVoice(ctx)
	posts, says := h.mt.got()
	if len(says) != 0 || len(posts) != 2 {
		t.Fatalf("posts %q says %q", posts, says)
	}
	if !contains(posts, "On are you there: "+hear) || !contains(posts, bp) {
		t.Fatalf("batched answers must each name their question: %q", posts)
	}

	// A single answer on its own stays as the persona wrote it.
	s.queueVoice(betty.Reply{Asker: "Andy Anderson", Topic: "status"}, "All green.")
	s.flushVoice(ctx)
	if posts, _ := h.mt.got(); posts[len(posts)-1] != "All green." {
		t.Fatalf("single answer = %q", posts[len(posts)-1])
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// speak says why it did not speak instead of a bare "voice unavailable".
func TestTrySpeakReasons(t *testing.T) {
	ctx := context.Background()
	h, s, _ := voiceSession(t, Policy{VoiceOnAddress: true})
	if reason, _, err := s.trySpeak(ctx, "hi"); reason != "" || err != nil {
		t.Fatalf("voice on = %q %v", reason, err)
	}
	h.m.VoiceEnabled = false
	h.mt.m[h.m.ID] = h.m
	if reason, _, _ := s.trySpeak(ctx, "hi"); reason != speakVoiceDisabled {
		t.Fatalf("voice off = %q", reason)
	}
	_, p, _ := voiceSession(t, Policy{})
	if reason, _, _ := p.trySpeak(ctx, "hi"); reason != speakPolicyOff {
		t.Fatalf("policy off = %q", reason)
	}
}

// A long spoken answer is said as its first sentence and the whole answer
// goes to chat once (first audio and the say deadline, live Meet test).
func TestLongAnswerSaidInBriefWithFullTextInChat(t *testing.T) {
	h, s, _ := voiceSession(t, Policy{ChatPostsPerMinute: 6, VoiceOnAddress: true})
	first := "Board pressure is twenty open issues and fifteen open PRs across fourteen repos."
	full := first + " Of those, eighteen are actionable right now and two are blocked."
	s.queueVoice(betty.Reply{}, full)
	s.flushVoice(context.Background())
	posts, says := h.mt.got()
	if len(says) != 1 || says[0] != first+" "+voiceRestInChat {
		t.Fatalf("says %q", says)
	}
	if len(posts) != 1 || posts[0] != full {
		t.Fatalf("posts %q, want the full answer once", posts)
	}
}
