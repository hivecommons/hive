package meetings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestJoinVoiceCapability(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	if err != nil || mt.VoiceCapable || q.joins[0].Voice {
		t.Fatalf("no voice: %+v %+v %v", mt, q.joins, err)
	}
	mt, _ = m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/y", Voice: true})
	if !mt.VoiceCapable || !mt.VoiceEnabled || !q.joins[1].Voice {
		t.Fatalf("voice join %+v", mt)
	}
	m.opts.VoiceOnAddress = true
	mt, _ = m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/z"})
	if !mt.VoiceCapable || mt.VoiceEnabled || !q.joins[2].Voice {
		t.Fatalf("voice-on-address join %+v", mt)
	}
}

func TestReplayFromLastSeq(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	mt, _ := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	ch := q.stream("q1")
	ch <- Event{Type: EventState, State: StateLive}
	ch <- Event{Type: EventSegment, Seq: 4, Speaker: &Person{Name: "Ana"}, Text: "one", Final: true}
	eventually(t, "seq stored", func() bool { got, _ := m.Get(mt.ID); return got.LastSeq == 4 })
	close(ch)
	eventually(t, "reconnect with after_seq", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.afters) >= 2 && q.afters[len(q.afters)-1] == 4
	})
	q.mu.Lock()
	next := q.streams["q1"]
	q.mu.Unlock()
	next <- Event{Type: EventSegment, Seq: 4, Speaker: &Person{Name: "Ana"}, Text: "one", Final: true}
	next <- Event{Type: EventSegment, Seq: 5, Speaker: &Person{Name: "Ana"}, Text: "two", Final: true}
	eventually(t, "two lines", func() bool { tr, _ := heardLines(m, mt.ID); return len(tr) == 2 })
	tr, _ := heardLines(m, mt.ID)
	if tr[1].Text != "two" {
		t.Fatalf("replayed duplicate stored: %+v", tr)
	}
}

func TestConsentErrorRecordedAndThrottled(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	m.opts.ConsentRetry = time.Hour
	// A transient failure (a rate limit) is recorded and retried later, not
	// on every line.
	q.chatErr = &ToolError{Code: CodeRateLimited, Message: "slow down"}
	mt, _ := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	ch := q.stream("q1")
	ch <- Event{Type: EventState, State: StateLive}
	eventually(t, "consent error", func() bool { got, _ := m.Get(mt.ID); return got.ConsentError != "" })
	ch <- Event{Type: EventChat, Author: &Person{Name: "Ana"}, Text: "hi"}
	// The line came before the notice: counted, never stored.
	eventually(t, "line dropped", func() bool { got, _ := m.Get(mt.ID); return got.PreConsentDropped == 1 })
	if got, _ := m.Get(mt.ID); got.ConsentPostedAt != nil || got.State != StateLive {
		t.Fatalf("a throttled retry must not post: %+v", got)
	}
	if tr, _ := heardLines(m, mt.ID); len(tr) != 0 {
		t.Fatalf("a pre-consent line was stored: %+v", tr)
	}
}

func TestConsentChatUnavailableWithoutVoiceLeaves(t *testing.T) {
	// Chat that cannot deliver the notice (platform_unavailable) on a
	// meeting without voice: hive leaves instead of listening untold.
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	q.chatErr = &ToolError{Code: CodePlatformUnavailable, Message: "chat not available"}
	mt, _ := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/x"})
	q.stream("q1") <- Event{Type: EventState, State: StateLive}
	eventually(t, "left", stateIs(m, mt.ID, StateEnded))
	got, _ := m.Get(mt.ID)
	if got.ConsentPostedAt != nil || got.StateReason != ReasonConsentUndelivered || got.ConsentError == "" {
		t.Fatalf("meeting %+v", got)
	}
}

func TestPlatformJoinRequests(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	var auto atomic.Int32
	m.opts.AutoApprove = func(p Person) bool { auto.Add(1); return p.ID == "owner-id" }
	m.StartRequests()
	all := q.stream("")
	all <- Event{Type: EventState, MeetingID: "q-d1", State: StateRequested, Platform: PlatformDiscord, GuildID: "g", ChannelID: "c", RequestedBy: &Person{ID: "u1", Name: "Ana"}}
	all <- Event{Type: EventState, MeetingID: "q-d1", State: StateRequested, Platform: PlatformDiscord, GuildID: "g", ChannelID: "c", RequestedBy: &Person{ID: "u1", Name: "Ana"}}
	all <- Event{Type: EventState, MeetingID: "q-x", State: StateLive}
	all <- Event{Type: EventState, State: StateRequested, RequestedBy: &Person{ID: "u2"}}
	// The reconnect snapshot of the same request (no requester) is a duplicate.
	all <- Event{Type: EventState, MeetingID: "q-d1b", State: StateRequested, GuildID: "g", ChannelID: "c"}
	var pending Meeting
	eventually(t, "pending request", func() bool {
		list, _ := m.List()
		for _, x := range list {
			if x.NativeID == "q-d1" {
				pending = x
				return true
			}
		}
		return false
	})
	if pending.State != StateRequested || pending.RequestedBy.Name != "Ana" || pending.InvitedBy != "Ana" || pending.Platform != PlatformDiscord {
		t.Fatalf("pending %+v", pending)
	}
	if list, _ := m.List(); len(list) != 1 {
		t.Fatalf("duplicate requests must not multiply: %d", len(list))
	}
	if _, err := m.DeclineRequest(context.Background(), "mtg-none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("decline missing = %v", err)
	}
	q.joinRes = JoinResult{MeetingID: "q-d1", State: StateJoining}
	got, err := m.ApproveRequest(context.Background(), pending.ID, true)
	if err != nil || got.State != StateJoining || !got.VoiceEnabled || !q.joins[len(q.joins)-1].Voice || q.joins[len(q.joins)-1].GuildID != "g" {
		t.Fatalf("approve = %+v, %v", got, err)
	}
	if _, err := m.ApproveRequest(context.Background(), pending.ID, false); !errors.Is(err, ErrNotRequested) {
		t.Fatalf("approve twice = %v", err)
	}
	if _, err := m.DeclineRequest(context.Background(), pending.ID); !errors.Is(err, ErrNotRequested) {
		t.Fatalf("decline approved = %v", err)
	}

	// A second request is declined.
	all <- Event{Type: EventState, MeetingID: "q-d2", State: StateRequested, GuildID: "g", ChannelID: "c2", RequestedBy: &Person{ID: "u3", Name: "Bo"}}
	var second Meeting
	eventually(t, "second request", func() bool {
		x, ok := m.findNative("q-d2")
		second = x
		return ok
	})
	q.leaveErr = &ToolError{Code: CodeUpstreamError}
	if _, err := m.DeclineRequest(context.Background(), second.ID); err == nil {
		t.Fatal("want the upstream error")
	}
	q.leaveErr = nil
	d, err := m.DeclineRequest(context.Background(), second.ID)
	if err != nil || d.State != StateFailed || d.StateReason != ReasonDeclined {
		t.Fatalf("decline = %+v, %v", d, err)
	}
	q.joinErr = errors.New("join failed")
	all <- Event{Type: EventState, MeetingID: "q-d3", State: StateRequested, GuildID: "g", ChannelID: "c3", RequestedBy: &Person{ID: "u4"}}
	var third Meeting
	eventually(t, "third request", func() bool { x, ok := m.findNative("q-d3"); third = x; return ok })
	if _, err := m.ApproveRequest(context.Background(), third.ID, false); err == nil {
		t.Fatal("want the join error")
	}
	// The failed approval keeps the request open and names why.
	if x, _ := m.Get(third.ID); x.State != StateRequested || x.StateReason != "join failed" {
		t.Fatalf("after a failed approve = %+v", x)
	}
	q.joinErr = nil
	q.joinRes = JoinResult{MeetingID: "q-d3", State: StateJoining}
	if x, err := m.ApproveRequest(context.Background(), third.ID, false); err != nil || x.State != StateJoining || x.StateReason != "" {
		t.Fatalf("retried approve = %+v, %v", x, err)
	}

	// A snapshot-only request (no requester) is still recorded.
	all <- Event{Type: EventState, MeetingID: "q-d5", State: StateRequested, GuildID: "g5", ChannelID: "c5"}
	eventually(t, "snapshot request", func() bool { x, ok := m.findNative("q-d5"); return ok && x.RequestedBy != nil })

	// An owner-mapped requester is approved automatically.
	q.joinRes = JoinResult{MeetingID: "q-d4", State: StateLive}
	all <- Event{Type: EventState, MeetingID: "q-d4", State: StateRequested, GuildID: "g", ChannelID: "c4", RequestedBy: &Person{ID: "owner-id", Name: "Owner"}}
	eventually(t, "auto approved", func() bool { x, ok := m.findNative("q-d4"); return ok && x.State == StateLive })

	// The request stream reconnects after a drop.
	q.mu.Lock()
	before := q.connects
	q.mu.Unlock()
	close(all)
	eventually(t, "request stream reconnect", func() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.connects > before })
	if auto.Load() == 0 {
		t.Fatal("auto-approve was never consulted")
	}
}

func TestRequestRecordFailure(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.opts.Store.dir = filepath.Join(blocker, "meetings")
	m.onRequest(Event{MeetingID: "q", RequestedBy: &Person{ID: "u"}})
	m.onRequest(Event{RequestedBy: &Person{ID: "u"}})
	if _, ok := m.findNative("q"); ok {
		t.Fatal("nothing can be recorded in a missing store")
	}
}

// A platform-initiated join (Discord `/hive join`) talks by default when the
// bot is voice-capable: the live Discord test joined with voice off and
// answered every spoken question in chat.
func TestPlatformJoinVoiceOnWhenCapable(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	m.opts.VoiceOnAddress = true
	m.opts.AutoApprove = func(p Person) bool { return p.ID == "owner-id" }
	m.StartRequests()
	all := q.stream("")
	q.joinRes = JoinResult{MeetingID: "q-v1", State: StateLive}
	all <- Event{Type: EventState, MeetingID: "q-v1", State: StateRequested, Platform: PlatformDiscord, GuildID: "g", ChannelID: "c", RequestedBy: &Person{ID: "owner-id", Name: "Owner"}}
	var got Meeting
	eventually(t, "auto approved", func() bool {
		x, ok := m.findNative("q-v1")
		got = x
		return ok && x.State == StateLive
	})
	q.mu.Lock()
	lastVoice := q.joins[len(q.joins)-1].Voice
	q.mu.Unlock()
	if !got.VoiceCapable || !got.VoiceEnabled || !lastVoice {
		t.Fatalf("auto-approved platform join must talk when capable: %+v", got)
	}
	// The owner can still turn it off.
	if off, err := m.SetVoice(got.ID, false); err != nil || off.VoiceEnabled {
		t.Fatalf("voice off = %+v, %v", off, err)
	}

	// A bot that cannot speak stays silent.
	m.opts.VoiceOnAddress = false
	all <- Event{Type: EventState, MeetingID: "q-v2", State: StateRequested, Platform: PlatformDiscord, GuildID: "g", ChannelID: "c2", RequestedBy: &Person{ID: "u9", Name: "Ana"}}
	var pending Meeting
	eventually(t, "second request", func() bool { x, ok := m.findNative("q-v2"); pending = x; return ok })
	q.joinRes = JoinResult{MeetingID: "q-v2", State: StateJoining}
	x, err := m.ApproveRequest(context.Background(), pending.ID, false)
	if err != nil || x.VoiceEnabled || x.VoiceCapable {
		t.Fatalf("not capable = %+v, %v", x, err)
	}
}
