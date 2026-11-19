package live

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// funcLLM answers through fn.
type funcLLM struct {
	fn func(ctx context.Context, prompt string) (string, error)
}

func (f funcLLM) Complete(ctx context.Context, _, prompt string) (string, error) {
	return f.fn(ctx, prompt)
}

// bettyHarness is personaHarness with adjustable engine settings and deps.
func bettyHarness(t *testing.T, p Policy, m meetings.Meeting, llm betty.LLM, tune func(*betty.Settings, *betty.Deps)) *harness {
	t.Helper()
	resolved := config.MeetingsPersonaConfig{}.Resolve()
	s := betty.SettingsFrom(resolved)
	s.AckDelay = 0
	s.AnswerWait, s.MinGap = personaWait, 0
	drops := &dropLog{m: map[string]int{}}
	h := newHarness(t, p, func(o *Options) {
		o.Persona = func(meetings.Meeting) *betty.Engine {
			d := betty.Deps{LLM: llm, Retrievers: []betty.Retriever{hitRetriever{}}, OnDrop: drops.add}
			st := s
			if tune != nil {
				tune(&st, &d)
			}
			return betty.New(st, d)
		}
		o.IsLeave = meetings.NewLeaveMatcher(resolved.LeavePhrases).Match
	})
	h.drops = drops
	h.m.Platform, h.m.VoiceEnabled, h.m.GuildID, h.m.ChannelID = m.Platform, m.VoiceEnabled, m.GuildID, m.ChannelID
	h.mt.m[h.m.ID] = h.m
	return h
}

func TestVoiceReplyAcknowledgesBeforeAnswer(t *testing.T) {
	const modelDelay = 400 * time.Millisecond
	llm := funcLLM{fn: func(ctx context.Context, prompt string) (string, error) {
		select {
		case <-time.After(modelDelay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return `{"answer":"The release ships Friday, details at https://x.example/release.","confidence":0.95}`, nil
	}}
	h := bettyHarness(t, Policy{ChatPostsPerMinute: 20, VoiceOnAddress: true},
		meetings.Meeting{Platform: meetings.PlatformGoogleMeet, VoiceEnabled: true}, llm, nil)
	h.live()
	start := time.Now()
	h.line(meetings.EventSegment, "Bo", "Betty, when does the release ship?", true)
	h.eventually("ack spoken", func() bool { _, s := h.mt.got(); return len(s) >= 1 })
	if d := time.Since(start); d >= modelDelay {
		t.Fatalf("the acknowledgement must not wait for the model: %v", d)
	}
	h.eventually("answer spoken", func() bool { _, s := h.mt.got(); return len(s) == 2 })
	posts, says := h.mt.got()
	if says[0] != "Let me check on that." || !strings.HasPrefix(says[1], "The release ships Friday") {
		t.Fatalf("says %q", says)
	}
	if strings.Contains(says[1], "http") {
		t.Fatalf("a spoken answer has no links: %q", says[1])
	}
	for _, p := range posts {
		if strings.Contains(p, "Let me check") || strings.Contains(p, "release ships") {
			t.Fatalf("spoken replies are not also posted: %q", posts)
		}
	}
	if st := h.state(); st.Intents != 1 {
		t.Fatalf("the acknowledgement is not charged: intents %d", st.Intents)
	}
}

// TestDiscordVoiceOffNeverSays pins hive's side of quorum v0.2.0 ignoring
// voice:false on Discord: with the meeting's voice off, neither the
// persona nor Spek may call say; answers go to chat.
func TestDiscordVoiceOffNeverSays(t *testing.T) {
	llm := &personaLLM{}
	discord := meetings.Meeting{Platform: meetings.PlatformDiscord, GuildID: "g", ChannelID: "c", VoiceEnabled: false}
	h := bettyHarness(t, Policy{ChatPostsPerMinute: 20, VoiceOnAddress: true}, discord, llm, nil)
	h.live()
	h.line(meetings.EventSegment, "Bo", "Betty, when does the release ship?", true)
	h.eventually("chat fallback", h.posted("The release ships Friday."))
	h.eventually("ack dropped", func() bool { return h.state().Dropped >= 1 })
	if _, says := h.mt.got(); len(says) != 0 {
		t.Fatalf("voice off on Discord must never say: %q", says)
	}
	if h.countPosts("Let me check") != 0 {
		t.Fatal("an acknowledgement never falls back to chat")
	}

	// Without the persona, Spek's say on the same meeting goes to chat.
	h2 := newHarness(t, Policy{VoiceOnAddress: true}, nil)
	h2.m.Platform, h2.m.GuildID, h2.m.ChannelID, h2.m.VoiceEnabled = meetings.PlatformDiscord, "g", "c", false
	h2.mt.m[h2.m.ID] = h2.m
	h2.live()
	h2.spek.intent(`{"type":"say","text":"spoken by spek"}`)
	h2.eventually("spek say in chat", h2.posted("spoken by spek"))
	if _, says := h2.mt.got(); len(says) != 0 {
		t.Fatalf("voice off on Discord must never say: %q", says)
	}
}

func TestAckSkippedPastBudget(t *testing.T) {
	h := bettyHarness(t, Policy{ChatPostsPerMinute: 20, VoiceOnAddress: true, MaxIntentsPerMeeting: 1},
		meetings.Meeting{Platform: meetings.PlatformGoogleMeet, VoiceEnabled: true}, &personaLLM{}, nil)
	h.live()
	h.spek.intent(`{"type":"chat.post","text":"one"}`)
	h.spek.intent(`{"type":"chat.post","text":"two"}`)
	h.eventually("budget", func() bool { return h.state().BudgetExhausted })
	h.line(meetings.EventSegment, "Bo", "Betty, when does the release ship?", true)
	h.dropped("budget")
	if _, says := h.mt.got(); len(says) != 0 {
		t.Fatalf("nothing is spoken past the budget: %q", says)
	}
}

func TestDeepDiveDispatchesAndFollowsUpOnce(t *testing.T) {
	llm := funcLLM{fn: func(_ context.Context, prompt string) (string, error) {
		if strings.Contains(prompt, "The research is done") {
			return "Update on the cert rotation: cert-manager renews it nightly.", nil
		}
		return `{"answer":"Not sure.","confidence":0.3,"needs_research":true}`, nil
	}}
	h := bettyHarness(t, Policy{ChatPostsPerMinute: 20}, meetings.Meeting{Platform: meetings.PlatformGoogleMeet}, llm,
		func(_ *betty.Settings, d *betty.Deps) { d.CanResearch = true })
	h.live()
	h.line(meetings.EventChat, "Bo", "Betty, how does the cert rotation work?", false)
	h.eventually("dig in", h.posted("I'll dig into that and report back."))
	h.eventually("dispatched", func() bool { r := h.state().Research; return len(r) == 1 && r[0].KickID != "" })
	it := h.state().Research[0]
	if !it.Betty || it.RID != "betty-1" || it.Query != "Betty, how does the cert rotation work?" || it.Status != ResearchQueued {
		t.Fatalf("deep dive item %+v", it)
	}
	h.res.mu.Lock()
	meta := h.res.metas[0]
	h.res.mu.Unlock()
	if meta["betty"] != "1" || meta["untrusted"] != "true" || meta["rid"] != "betty-1" {
		t.Fatalf("meta %+v", meta)
	}
	if h.spek.has("betty-1") {
		t.Fatal("Spek never hears of the persona's deep dives")
	}
	h.r.DeliverResearch(ResearchSignal{KickID: it.KickID, Kind: SignalFinding, Text: "cert-manager"})
	h.r.DeliverResearch(ResearchSignal{KickID: it.KickID, Kind: SignalComplete, Text: "cert-manager renews it every night."})
	h.eventually("follow up", h.posted("Update on the cert rotation"))
	h.eventually("done", func() bool { return h.state().Research[0].Status == ResearchDone })
	if h.r.DeliverResearch(ResearchSignal{KickID: it.KickID, Kind: SignalComplete, Text: "again"}) {
		t.Fatal("a completed deep dive is unregistered")
	}
	if n := h.countPosts("Update on the cert rotation"); n != 1 {
		t.Fatalf("exactly one follow-up, got %d", n)
	}
	if h.countPosts(`Research on "`) != 0 {
		t.Fatal("the raw research summary is not posted for a deep dive")
	}
}

func TestDeepDivePastCapWaitsForApproval(t *testing.T) {
	llm := funcLLM{fn: func(context.Context, string) (string, error) {
		return `{"answer":"Not sure.","confidence":0.3,"needs_research":true}`, nil
	}}
	h := bettyHarness(t, Policy{ChatPostsPerMinute: 20, ResearchPerMeeting: 1}, meetings.Meeting{Platform: meetings.PlatformGoogleMeet}, llm,
		func(_ *betty.Settings, d *betty.Deps) { d.CanResearch = true })
	h.live()
	h.spek.intent(`{"type":"research","rid":"r1","query":"spek research"}`)
	h.eventually("spek research", func() bool { return len(h.state().Research) == 1 })
	h.line(meetings.EventChat, "Bo", "Betty, how does the cert rotation work?", false)
	h.eventually("pending", func() bool {
		r := h.state().Research
		return len(r) == 2 && r[1].Betty && r[1].Status == ResearchPendingApproval
	})
	if err := h.r.Approve(h.m.ID, "betty-1"); err != nil {
		t.Fatal(err)
	}
	h.eventually("approved", func() bool { return h.state().Research[1].Status == ResearchQueued })
}

func TestRestyleIsAsyncAndKeepsOrder(t *testing.T) {
	release := make(chan struct{})
	llm := funcLLM{fn: func(ctx context.Context, prompt string) (string, error) {
		if strings.Contains(prompt, "Rewrite this clarifying question") {
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "Quick check: which Friday?", nil
		}
		return `{"answer":"x","confidence":0.1}`, nil
	}}
	h := bettyHarness(t, Policy{ChatPostsPerMinute: 20}, meetings.Meeting{Platform: meetings.PlatformGoogleMeet}, llm,
		func(s *betty.Settings, _ *betty.Deps) { s.RestyleTimeout = 5 * time.Second })
	h.live()
	h.spek.intent(`{"type":"ask","qid":"q-1","text":"Which friday?"}`)
	h.spek.intent(`{"type":"chat.post","text":"Action item: Bo drafts the changelog."}`)
	// The session keeps serving while the restyle runs.
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"kept"}`)
	h.eventually("notes while restyling", func() bool { return len(h.state().Notes[SectionNotes]) == 1 })
	if p, _ := h.mt.got(); len(p) != 0 {
		t.Fatalf("nothing posts ahead of the pending ask: %q", p)
	}
	close(release)
	h.eventually("both posted", func() bool { p, _ := h.mt.got(); return len(p) == 2 })
	posts, _ := h.mt.got()
	if posts[0] != "Quick check: which Friday?" || posts[1] != "Action item: Bo drafts the changelog." {
		t.Fatalf("order %q", posts)
	}
	if a := h.state().Asks; len(a) != 1 || a[0].QID != "q-1" || a[0].Text != "Quick check: which Friday?" {
		t.Fatalf("asks %+v", a)
	}
	// A chat post with nothing pending goes straight to the persona.
	h.spek.intent(`{"type":"chat.post","text":"Decision: freeze on Thursday."}`)
	h.eventually("direct offer", h.posted("Decision: freeze on Thursday."))
}

func TestRestyleTimeoutPostsRaw(t *testing.T) {
	llm := funcLLM{fn: func(ctx context.Context, prompt string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	h := bettyHarness(t, Policy{ChatPostsPerMinute: 20}, meetings.Meeting{Platform: meetings.PlatformGoogleMeet}, llm,
		func(s *betty.Settings, _ *betty.Deps) { s.RestyleTimeout = 50 * time.Millisecond })
	h.live()
	h.spek.intent(`{"type":"ask","qid":"q-1","text":"Which friday?"}`)
	h.spek.intent(`{"type":"ask","qid":"q-2","text":"Which branch?"}`)
	h.eventually("raw asks", func() bool { p, _ := h.mt.got(); return len(p) == 2 })
	if posts, _ := h.mt.got(); posts[0] != "Which friday?" || posts[1] != "Which branch?" {
		t.Fatalf("raw text in order on timeout: %q", posts)
	}
}
