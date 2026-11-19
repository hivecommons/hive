package meetings

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A reply line is stored in the transcript beside the people's lines, in
// order, without a Quorum seq, and reads back with its details.
func TestAppendReplyRoundTrip(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "mtg-1"
	if err := store.Save(Meeting{ID: id, State: StateLive, LastSeq: 7}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTranscript(id, Event{Type: EventSegment, Seq: 7, Speaker: &Person{Name: "Ana"}, Text: "Betty, are you there?", Final: true}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 17, 15, 52, 30, 0, time.UTC)
	timing, _ := json.Marshal(ReplyTiming{HeardAt: at, DeliverMS: 40, TotalMS: 900})
	stored, err := store.AppendReply(id, "Betty", "Yes, I can hear you.", at, ReplyInfo{
		Kind: ReplyKindVoice, Trigger: "addressed", Source: "model", Question: "Betty, are you there?", Delivered: true, Timing: timing,
	})
	if err != nil || stored.Type != EventReply {
		t.Fatalf("append %+v %v", stored, err)
	}
	if _, err := store.AppendReply(id, "Betty", "It ships Friday.", at, ReplyInfo{Kind: ReplyKindChat, Trigger: "addressed", Undelivered: "chat down"}); err != nil {
		t.Fatal(err)
	}
	tr, err := store.Transcript(id)
	if err != nil || len(tr) != 3 {
		t.Fatalf("transcript %+v %v", tr, err)
	}
	if tr[0].IsReply() || !tr[1].IsReply() || !tr[2].IsReply() {
		t.Fatalf("order %+v", tr)
	}
	r := tr[1]
	if r.Seq != 0 || r.Who() != "Betty" || r.Text != "Yes, I can hear you." || r.Reply == nil || r.Reply.Kind != ReplyKindVoice ||
		r.Reply.Source != "model" || !r.Reply.Delivered {
		t.Fatalf("reply %+v", r)
	}
	var got ReplyTiming
	if err := json.Unmarshal(r.Reply.Timing, &got); err != nil || !got.HeardAt.Equal(at) || got.TotalMS != 900 {
		t.Fatalf("timing %+v %v", got, err)
	}
	var when time.Time
	if err := json.Unmarshal(r.At, &when); err != nil || !when.Equal(at) {
		t.Fatalf("at %s %v", r.At, err)
	}
	if got := r.ReplyLabel(); got != "Betty (assistant, spoken; source: model)" {
		t.Fatalf("label %q", got)
	}
	if got := tr[2].ReplyLabel(); got != "Betty (assistant, not delivered: chat down)" {
		t.Fatalf("undelivered label %q", got)
	}
	// The replay cursor is keyed on Quorum's seq and is not moved.
	if m, _ := store.Get(id); m.LastSeq != 7 {
		t.Fatalf("last seq %d", m.LastSeq)
	}
	// Her lines follow the transcript's retention: purged with it.
	if err := store.PurgeTranscript(id); err != nil {
		t.Fatal(err)
	}
	if tr, _ := store.Transcript(id); len(tr) != 0 {
		t.Fatalf("purge left %d lines", len(tr))
	}
}

// RecordReply keeps a line the manager said, delivered or not.
func TestManagerRecordReply(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)
	m := &Manager{opts: Options{Store: store, BotName: "Betty", Now: func() time.Time { return now }}, log: slog.New(slog.DiscardHandler)}
	const id = "mtg-1"
	if err := store.Save(Meeting{ID: id, State: StateLive}); err != nil {
		t.Fatal(err)
	}
	heard := now.Add(-2 * time.Second)
	m.RecordReply(id, LeavingText, ReplyKindChat, ReplyTriggerLeave, heard, 300*time.Millisecond, nil)
	m.RecordReply(id, LeavingText, ReplyKindVoice, ReplyTriggerLeave, time.Time{}, time.Second, errors.New("tts down"))
	tr, err := store.Transcript(id)
	if err != nil || len(tr) != 2 {
		t.Fatalf("transcript %+v %v", tr, err)
	}
	first := tr[0]
	if !first.IsReply() || first.Who() != "Betty" || first.Text != LeavingText || first.Reply.Trigger != ReplyTriggerLeave ||
		first.Reply.Source != ReplySourceFixed || !first.Reply.Delivered {
		t.Fatalf("first %+v", first)
	}
	var tm ReplyTiming
	if err := json.Unmarshal(first.Reply.Timing, &tm); err != nil || !tm.HeardAt.Equal(heard) || tm.DeliverMS != 300 || tm.TotalMS != 2000 {
		t.Fatalf("timing %+v %v", tm, err)
	}
	if second := tr[1]; second.Reply.Delivered || !strings.Contains(second.Reply.Undelivered, "tts down") || second.Reply.Kind != ReplyKindVoice {
		t.Fatalf("second %+v", second.Reply)
	}
}
