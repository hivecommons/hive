package live

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// Deep dive follow-through (hivecommons/hive#18590 findings 5 and 6): once
// Betty says she will dig in, she always closes the loop in the channel she
// was asked in: the answer, a progress note when it runs long, or why she
// could not.

// reasonErr is a dispatch error carrying a sayable reason, like
// research.ErrNoAgents.
type reasonErr struct{ reason string }

func (e reasonErr) Error() string  { return "research pool: " + e.reason }
func (e reasonErr) Reason() string { return e.reason }

// digLLM never knows the answer, so an addressed question becomes a deep
// dive; it restyles a finished one as "Update: <summary>".
var digLLM = funcLLM{fn: func(_ context.Context, prompt string) (string, error) {
	if strings.Contains(prompt, "The research is done") {
		return "Update: cert-manager renews it nightly.", nil
	}
	return `{"answer":"Not sure.","confidence":0.3,"needs_research":true}`, nil
}}

// deepDiveHarness is bettyHarness with deep dives on and the follow-through
// timings set.
func deepDiveHarness(t *testing.T, m meetings.Meeting, progress, timeout time.Duration) *harness {
	t.Helper()
	resolved := config.MeetingsPersonaConfig{}.Resolve()
	s := betty.SettingsFrom(resolved)
	s.AckDelay = 0
	s.AnswerWait, s.MinGap = personaWait, 0
	drops := &dropLog{m: map[string]int{}}
	h := newHarness(t, Policy{ChatPostsPerMinute: 20, VoiceOnAddress: true}, func(o *Options) {
		o.Persona = func(meetings.Meeting) *betty.Engine {
			return betty.New(s, betty.Deps{LLM: digLLM, Retrievers: []betty.Retriever{hitRetriever{}}, OnDrop: drops.add, CanResearch: true})
		}
		o.IsLeave = meetings.NewLeaveMatcher(resolved.LeavePhrases).Match
		o.ResearchProgressAfter, o.ResearchTimeout, o.ResearchCheckEvery = progress, timeout, 10*time.Millisecond
	})
	h.drops = drops
	h.m.Platform, h.m.VoiceEnabled = m.Platform, m.VoiceEnabled
	h.mt.m[h.m.ID] = h.m
	return h
}

func (h *harness) said(sub string) func() bool {
	return func() bool {
		_, says := h.mt.got()
		for _, x := range says {
			if strings.Contains(x, sub) {
				return true
			}
		}
		return false
	}
}

func TestDeepDiveDispatchFailureIsSaidInChat(t *testing.T) {
	h := deepDiveHarness(t, meetings.Meeting{Platform: meetings.PlatformGoogleMeet}, time.Hour, time.Hour)
	h.res.err = reasonErr{reason: "no research agent is available on this hive"}
	h.live()
	h.line(meetings.EventChat, "Bo", "Betty, how does the cert rotation work?", false)
	h.eventually("dig in", h.posted("I'll dig into that and report back."))
	h.eventually("failure said", h.posted("I couldn't look that up: no research agent is available on this hive."))
	it := h.state().Research[0]
	if it.Status != ResearchFailed || !it.Posted {
		t.Fatalf("item %+v", it)
	}
	if n := h.countPosts("I couldn't look that up"); n != 1 {
		t.Fatalf("exactly one failure notice, got %d", n)
	}
	if _, says := h.mt.got(); len(says) != 0 {
		t.Fatalf("a typed question is answered in chat only: %q", says)
	}
}

func TestDeepDiveFailureIsSpokenWhenAskedByVoice(t *testing.T) {
	h := deepDiveHarness(t, meetings.Meeting{Platform: meetings.PlatformGoogleMeet, VoiceEnabled: true}, time.Hour, time.Hour)
	h.res.err = errors.New("queue full")
	h.live()
	h.line(meetings.EventSegment, "Bo", "Betty, how does the cert rotation work?", true)
	// She already said "one moment" aloud: the dig-in goes to chat, not a
	// second spoken ack back to back.
	h.eventually("dig in posted", h.posted("I'll dig into that and report back."))
	if _, says := h.mt.got(); func() bool {
		for _, x := range says {
			if strings.Contains(x, "I'll dig into that") {
				return true
			}
		}
		return false
	}() {
		t.Fatal("the dig-in must not be spoken after the spoken ack")
	}
	h.eventually("failure spoken", h.said("I couldn't look that up: the research could not be started."))
	if !h.state().Research[0].Voice {
		t.Fatal("a spoken deep dive is marked voice")
	}
	if h.countPosts("I couldn't look that up") != 0 {
		t.Fatal("a spoken failure is not also posted")
	}
}

func TestDeepDiveAgentErrorIsSaid(t *testing.T) {
	h := deepDiveHarness(t, meetings.Meeting{Platform: meetings.PlatformGoogleMeet}, time.Hour, time.Hour)
	h.live()
	h.line(meetings.EventChat, "Bo", "Betty, how does the cert rotation work?", false)
	h.eventually("dispatched", func() bool { r := h.state().Research; return len(r) == 1 && r[0].KickID != "" })
	h.r.DeliverResearch(ResearchSignal{KickID: h.state().Research[0].KickID, Kind: SignalError, Text: "the docs site is down"})
	h.eventually("error said", h.posted("I couldn't look that up: the docs site is down."))
}

func TestDeepDiveProgressNoteThenTimeout(t *testing.T) {
	const progress, timeout = 100 * time.Millisecond, 400 * time.Millisecond
	h := deepDiveHarness(t, meetings.Meeting{Platform: meetings.PlatformGoogleMeet}, progress, timeout)
	h.live()
	h.line(meetings.EventChat, "Bo", "Betty, how does the cert rotation work?", false)
	h.eventually("dispatched", func() bool { r := h.state().Research; return len(r) == 1 && r[0].KickID != "" })
	h.eventually("progress note", h.posted("Still looking into that"))
	h.eventually("timeout said", h.posted("I couldn't look that up: it took too long."))
	it := h.state().Research[0]
	if it.Status != ResearchFailed || !it.ProgressSent {
		t.Fatalf("item %+v", it)
	}
	if n := h.countPosts("Still looking into that"); n != 1 {
		t.Fatalf("one progress note, got %d", n)
	}
	// A late answer after the give-up is not routed: the loop is closed.
	if h.r.DeliverResearch(ResearchSignal{KickID: it.KickID, Kind: SignalComplete, Text: "late"}) {
		t.Fatal("a timed-out deep dive is unregistered")
	}
}

func TestSpokenDeepDiveFollowsUpByVoice(t *testing.T) {
	h := deepDiveHarness(t, meetings.Meeting{Platform: meetings.PlatformGoogleMeet, VoiceEnabled: true}, time.Hour, time.Hour)
	h.live()
	h.line(meetings.EventSegment, "Bo", "Betty, how does the cert rotation work?", true)
	h.eventually("dispatched", func() bool { r := h.state().Research; return len(r) == 1 && r[0].KickID != "" })
	h.r.DeliverResearch(ResearchSignal{KickID: h.state().Research[0].KickID, Kind: SignalComplete, Text: "cert-manager renews it every night."})
	h.eventually("follow up spoken", h.said("Update: cert-manager renews it nightly."))
	if h.countPosts("Update:") != 0 {
		t.Fatal("a spoken question's follow-up is spoken, not posted")
	}
}

func TestSpekResearchFailureStaysOnThePanel(t *testing.T) {
	h := deepDiveHarness(t, meetings.Meeting{Platform: meetings.PlatformGoogleMeet}, time.Hour, time.Hour)
	h.res.err = errors.New("boom")
	h.live()
	h.spek.intent(`{"type":"research","rid":"r1","query":"what is vexa"}`)
	h.eventually("failed", func() bool { r := h.state().Research; return len(r) == 1 && r[0].Status == ResearchFailed })
	if h.countPosts("I couldn't look that up") != 0 {
		t.Fatal("Spek's research failures are not announced by the persona")
	}
}

func TestResearchFailReason(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{reasonErr{reason: "no agent"}, "no agent"},
		{context.DeadlineExceeded, "the research agents did not answer in time"},
		{errors.New("x"), "the research could not be started"},
	} {
		if got := researchFailReason(c.err); got != c.want {
			t.Errorf("researchFailReason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
