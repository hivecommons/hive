package live

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/actions"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// draftSink records the persona's drafts.
type draftSink struct {
	mu  sync.Mutex
	got []actions.Draft
}

func (d *draftSink) Settings() actions.Settings {
	return actions.Settings{Enabled: true, Types: []actions.Type{actions.TypeIssue}, DefaultRepo: "hivecommons/hive"}
}

func (d *draftSink) Submit(x actions.Draft) (actions.Draft, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	x.ID, x.Status = "act_1", actions.StatusPending
	d.got = append(d.got, x)
	return x, nil
}

func (d *draftSink) n() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.got)
}

func TestPersonaActionRequestThroughSession(t *testing.T) {
	h := personaHarness(t, Policy{ChatPostsPerMinute: 20}, config.MeetingsPersonaConfig{}, &personaLLM{})
	sink := &draftSink{}
	var (
		mu   sync.Mutex
		seen []string
	)
	h.r.SetPersonaActions(func(m meetings.Meeting) betty.ActionSink {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, m.ID)
		return sink
	})
	h.live()
	h.line(meetings.EventChat, "Bo", "Betty, open an issue for the lobby timeout", false)
	h.eventually("draft confirmation", h.posted("Drafted an issue: 'The lobby timeout'. It's waiting for approval in the hive."))
	mu.Lock()
	defer mu.Unlock()
	if sink.n() != 1 || len(seen) != 1 || seen[0] != h.m.ID {
		t.Fatalf("sink %d seen %v", sink.n(), seen)
	}
	// The confirmation is a persona reply through the usual chat path.
	h.eventually("counted", func() bool { return h.state().PersonaReplies >= 1 })
}

func TestPersonaActionsOffWithoutFactory(t *testing.T) {
	h := personaHarness(t, Policy{ChatPostsPerMinute: 20}, config.MeetingsPersonaConfig{}, &personaLLM{})
	h.r.SetPersonaActions(func(meetings.Meeting) betty.ActionSink { return nil })
	h.live()
	h.line(meetings.EventChat, "Bo", "Betty, open an issue for the release", false)
	// No sink: an ordinary addressed answer.
	h.eventually("addressed answer", h.posted("The release ships Friday."))
	if h.countPosts("Drafted") != 0 {
		t.Fatal("no drafts without a sink")
	}
	// A nil engine is left alone.
	h.r.attachActions(nil, h.m)
}

func TestAnnounceLiveChat(t *testing.T) {
	h := newHarness(t, Policy{ChatPostsPerMinute: 20}, nil)
	h.live()
	where, err := h.r.Announce(context.Background(), h.m.ID, "Approved: an issue \"x\" is open: https://x/1")
	if err != nil || where != AnnouncedChat {
		t.Fatalf("announce %q %v", where, err)
	}
	h.eventually("posted", h.posted("is open: https://x/1"))
	if where, err := h.r.Announce(context.Background(), h.m.ID, "  "); where != "" || err != nil {
		t.Fatalf("empty %q %v", where, err)
	}
	h.mt.mu.Lock()
	h.mt.postErr = errors.New("platform_unavailable")
	h.mt.mu.Unlock()
	if _, err := h.r.Announce(context.Background(), h.m.ID, "again"); !errors.Is(err, ErrChatUnavailable) {
		t.Fatalf("chat down = %v", err)
	}
}

func TestAnnounceEndedGoesToNotes(t *testing.T) {
	h := newHarness(t, Policy{}, func(o *Options) { o.PersonaName = "Betty" })
	h.live()
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"before end"}`)
	h.eventually("note", func() bool { return len(h.state().Notes[SectionNotes]) == 1 })
	ended := h.m
	ended.State = meetings.StateEnded
	h.mt.push(meetings.Update{Meeting: ended})
	h.mt.push(meetings.Update{Meeting: ended})
	// While the session closes, Announce waits for it, then writes notes.
	where, err := h.r.Announce(context.Background(), h.m.ID, "Saved the knowledge note \"x\" to the hive.")
	if err != nil || where != AnnouncedNotes {
		t.Fatalf("announce %q %v", where, err)
	}
	st, err := h.r.State(h.m.ID)
	if err != nil {
		t.Fatal(err)
	}
	notes := st.Notes[SectionNotes]
	if len(notes) != 2 || notes[0] != "before end" || !strings.Contains(notes[1], "Saved the knowledge note") {
		t.Fatalf("notes %q", notes)
	}
	h.notes.mu.Lock()
	body := h.notes.facts[NotesTitle(ended)]
	h.notes.mu.Unlock()
	if !strings.Contains(body, "Saved the knowledge note") || !strings.Contains(body, "Notes Betty took") {
		t.Fatalf("notes fact %q", body)
	}
}

func TestAnnounceUnknownMeetingAndNoSink(t *testing.T) {
	h := newHarness(t, Policy{}, func(o *Options) { o.Notes = nil })
	// A meeting the hive does not know is refused; no stray state is made.
	if _, err := h.r.Announce(context.Background(), "mtg-old", "done"); !errors.Is(err, meetings.ErrNotFound) {
		t.Fatalf("unknown meeting = %v", err)
	}
	// An ended meeting without a notes sink keeps the note in its state.
	ended := h.m
	ended.State = meetings.StateEnded
	h.mt.mu.Lock()
	h.mt.m[h.m.ID] = ended
	h.mt.mu.Unlock()
	if where, err := h.r.Announce(context.Background(), h.m.ID, "done"); err != nil || where != AnnouncedNotes {
		t.Fatalf("announce %q %v", where, err)
	}
	var st State
	if err := h.store.LoadDoc(h.m.ID, StateDoc, &st); err != nil || len(st.Notes[SectionNotes]) != 1 {
		t.Fatalf("stored %+v %v", st, err)
	}
	// The session is still running for an ended meeting: a cancelled caller
	// gives up instead of racing its last save.
	h.mt.mu.Lock()
	h.mt.m[h.m.ID] = h.m
	h.mt.mu.Unlock()
	h.live()
	h.mt.mu.Lock()
	h.mt.m[h.m.ID] = ended
	h.mt.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.r.Announce(ctx, h.m.ID, "late"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want the context error, got %v", err)
	}
}
