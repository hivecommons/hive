package meetings

import (
	"testing"
	"time"
)

const echoIntro = "I'm Hive, taking notes. Say 'hive leave' to remove me."

// Her own introduction names the leave phrase. When the platform shows her
// post under a name that is not the bot's, the line must not be read as a
// participant asking her to leave.
func TestOwnLineEchoedUnderAnotherNameIsNotALeave(t *testing.T) {
	q := &confirmingQuorum{fakeQuorum: newFakeQuorum()}
	m, mt := liveTeams(t, q, false)
	eventually(t, "consent", func() bool { got, _ := m.Get(mt.ID); return got.ConsentPostedAt != nil })

	m.RecordReply(mt.ID, echoIntro, ReplyKindChat, "intro", time.Time{}, 0, nil)
	ch := q.stream("q1")
	ch <- Event{Type: EventChat, Seq: 50, Author: &Person{Name: "Guest 4121"}, Text: "  I'm HIVE, taking notes.\nSay 'hive leave' to remove me. "}
	// A real participant's identical request still works afterwards.
	ch <- Event{Type: EventChat, Seq: 51, Author: &Person{Name: "Ana"}, Text: "hive leave"}
	eventually(t, "left on the participant's request", stateIs(m, mt.ID, StateEnded))
	got, _ := m.Get(mt.ID)
	if got.StateReason != ReasonHiveLeave {
		t.Fatalf("meeting %+v", got)
	}
	tr, _ := heardLines(m, mt.ID)
	for _, ev := range tr {
		if ev.Seq == 50 {
			t.Fatalf("her own line came back as a participant's: %+v", ev)
		}
	}
	if len(tr) < 1 || tr[len(tr)-1].Seq != 51 {
		t.Fatalf("transcript %+v: the participant's own leave request must still be heard", tr)
	}
}

func TestOwnEchoWindowAndKey(t *testing.T) {
	var o ownEchoes
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	o.remember("m1", "Hello   There", at)
	cases := []struct {
		id, text string
		now      time.Time
		want     bool
	}{
		{"m1", "hello there", at.Add(time.Second), true},
		{"m1", "hello there", at.Add(ownEchoWindow), true},
		{"m1", "hello there", at.Add(ownEchoWindow + time.Second), false},
		{"m2", "hello there", at.Add(time.Second), false},
		{"m1", "hello", at.Add(time.Second), false},
		{"m1", "   ", at.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := o.match(c.id, c.text, c.now); got != c.want {
			t.Errorf("match(%q, %q, +%v) = %v, want %v", c.id, c.text, c.now.Sub(at), got, c.want)
		}
	}
}
