package meetings

import (
	"context"
	"errors"
	"sync"
	"testing"
)

const (
	ownerDiscordID = "900000000000000001"
	otherDiscordID = "900000000000000002"
)

// trustedManager starts the request stream with a trust store and an audit
// recorder.
func trustedManager(t *testing.T) (*Manager, *fakeQuorum, *TrustStore, func() []string) {
	t.Helper()
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	tr := NewTrustStore(t.TempDir())
	m.opts.Trust = tr
	var mu sync.Mutex
	var audited []string
	m.opts.OnAutoApproved = func(mt Meeting, who Person) {
		mu.Lock()
		defer mu.Unlock()
		audited = append(audited, who.ID+"|"+mt.ID)
	}
	m.StartRequests()
	return m, q, tr, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), audited...) }
}

func discordRequest(native, id, name string) Event {
	return Event{Type: EventState, MeetingID: native, State: StateRequested, Platform: PlatformDiscord,
		GuildID: "g", ChannelID: "c-" + native, RequestedBy: &Person{ID: id, Name: name}}
}

func pendingBy(m *Manager, native string) func() bool {
	return func() bool {
		mt, ok := m.findNative(native)
		return ok && mt.State == StateRequested
	}
}

func TestTrustedOwnerIDAutoApprovesAndAudits(t *testing.T) {
	m, q, tr, audited := trustedManager(t)
	all := q.stream("")
	all <- discordRequest("q-first", ownerDiscordID, "andy")
	eventually(t, "first request waits", pendingBy(m, "q-first"))
	first, _ := m.findNative("q-first")
	q.joinRes = JoinResult{MeetingID: "q-first", State: StateJoining}
	if _, tru, err := m.ApproveAndTrust(context.Background(), first.ID, false, "clubanderson"); err != nil || tru.ID != ownerDiscordID || tru.By != "clubanderson" {
		t.Fatalf("approve and trust: %+v %v", tru, err)
	}
	if !tr.IsTrusted(PlatformDiscord, ownerDiscordID) {
		t.Fatal("owner id should be trusted")
	}
	q.joinRes = JoinResult{MeetingID: "q-second", State: StateJoining}
	all <- discordRequest("q-second", ownerDiscordID, "andy")
	eventually(t, "second request approved", func() bool {
		mt, ok := m.findNative("q-second")
		return ok && mt.State == StateJoining
	})
	mt, _ := m.findNative("q-second")
	if got := audited(); len(got) != 1 || got[0] != ownerDiscordID+"|"+mt.ID {
		t.Fatalf("audit = %v", got)
	}
}

func TestSameDisplayNameDifferentIDDoesNotAutoApprove(t *testing.T) {
	m, q, tr, audited := trustedManager(t)
	if _, err := tr.Add(Trusted{Platform: PlatformDiscord, ID: ownerDiscordID, Name: "andy"}); err != nil {
		t.Fatal(err)
	}
	q.stream("") <- discordRequest("q-imp", otherDiscordID, "andy")
	eventually(t, "impostor waits", pendingBy(m, "q-imp"))
	if len(q.joins) != 0 || len(audited()) != 0 {
		t.Fatalf("a matching name must not approve: joins=%v audit=%v", q.joins, audited())
	}
}

func TestRemovedTrustDoesNotAutoApprove(t *testing.T) {
	m, q, tr, audited := trustedManager(t)
	if _, err := tr.Add(Trusted{Platform: PlatformDiscord, ID: ownerDiscordID}); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.Untrust(PlatformDiscord, ownerDiscordID); !ok || err != nil {
		t.Fatalf("untrust = %v %v", ok, err)
	}
	q.stream("") <- discordRequest("q-gone", ownerDiscordID, "andy")
	eventually(t, "request waits", pendingBy(m, "q-gone"))
	if len(q.joins) != 0 || len(audited()) != 0 {
		t.Fatalf("removed trust approved: joins=%v", q.joins)
	}
}

func TestTrustNeedsARealDiscordID(t *testing.T) {
	m, q, tr, _ := trustedManager(t)
	q.stream("") <- discordRequest("q-name", "andy", "andy")
	eventually(t, "request waits", pendingBy(m, "q-name"))
	mt, _ := m.findNative("q-name")
	if _, _, err := m.ApproveAndTrust(context.Background(), mt.ID, false, "o"); !errors.Is(err, ErrTrustInvalid) {
		t.Fatalf("non-snowflake id trusted: %v", err)
	}
	if len(q.joins) != 0 {
		t.Fatal("a refused trust must not join")
	}
	if _, err := tr.Add(Trusted{Platform: "zoom", ID: ownerDiscordID}); !errors.Is(err, ErrTrustInvalid) {
		t.Fatalf("non-discord trusted: %v", err)
	}
}
