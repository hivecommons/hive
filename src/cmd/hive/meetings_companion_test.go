package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings"
)

// companionStubQuorum is stubQuorum plus the companion routes; joins start
// live companion meetings.
type companionStubQuorum struct {
	stubQuorum
	gap bool
}

func (q *companionStubQuorum) Join(context.Context, meetings.JoinArgs) (meetings.JoinResult, error) {
	return meetings.JoinResult{MeetingID: "qc", Platform: meetings.PlatformCompanion, State: meetings.StateLive}, nil
}

func (q *companionStubQuorum) CompanionAudio(_ context.Context, _, _ string, seq int, _ []byte) (meetings.CompanionChunk, error) {
	if q.gap {
		return meetings.CompanionChunk{}, &meetings.ChunkGapError{Expected: 2}
	}
	return meetings.CompanionChunk{Seq: seq}, nil
}

func (q *companionStubQuorum) CompanionOutbox(context.Context, string, int64) (meetings.CompanionPage, error) {
	return meetings.CompanionPage{Lines: []meetings.CompanionLine{{Seq: 1, Text: "consent"}}, NextSeq: 1}, nil
}

func TestMeetingsDashboardCompanion(t *testing.T) {
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := &companionStubQuorum{stubQuorum: stubQuorum{events: make(chan meetings.Event, 8)}}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: q, ConsentText: "consent"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	d := &meetingsDashboard{mgr: mgr}
	ctx := context.Background()

	info, ok := d.CompanionInfo().(companionInfo)
	if !ok || info.ConsentVersion != meetings.CompanionConsentVersion || info.SampleRate != meetings.CompanionSampleRate || len(info.Channels) != 2 {
		t.Fatalf("info = %+v", d.CompanionInfo())
	}
	if _, err := d.StartCompanion(ctx, dashboard.CompanionStartRequest{ConsentVersion: "old", ConsentAttested: true}, "ana"); !errors.Is(err, dashboard.ErrMeetingInvalid) {
		t.Fatalf("old consent = %v", err)
	}
	v, err := d.StartCompanion(ctx, dashboard.CompanionStartRequest{
		Title: "Standup", App: "zoom", ConsentVersion: meetings.CompanionConsentVersion, ConsentAttested: true,
	}, "ana")
	if err != nil {
		t.Fatal(err)
	}
	mt, ok := v.(meetings.Meeting)
	if !ok || mt.Platform != meetings.PlatformCompanion {
		t.Fatalf("start = %+v", v)
	}
	if _, err := d.CompanionAudio(ctx, mt.ID, "ana", false, "mic", 0, []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompanionAudio(ctx, mt.ID, "bob", false, "mic", 0, nil); !errors.Is(err, dashboard.ErrMeetingForbidden) {
		t.Fatalf("other user = %v", err)
	}
	q.gap = true
	var gap *dashboard.CompanionGapError
	if _, err := d.CompanionAudio(ctx, mt.ID, "ana", false, "call", 5, nil); !errors.As(err, &gap) || gap.Expected != 2 {
		t.Fatalf("gap = %v", err)
	}
	if r, err := d.CompanionReplies(ctx, mt.ID, "ana", false, 0); err != nil || r.(meetings.CompanionReplies).NextSeq != 1 {
		t.Fatalf("replies = %+v, %v", r, err)
	}
	if _, err := d.CompanionReplies(ctx, "mtg-missing", "ana", false, 0); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if v, err := d.EndCompanion(ctx, mt.ID, "ana", false); err != nil || v.(meetings.Meeting).State != meetings.StateEnded {
		t.Fatalf("end = %+v, %v", v, err)
	}
	if _, err := d.EndCompanion(ctx, "mtg-missing", "ana", false); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("end missing = %v", err)
	}

	plain := &meetingsDashboard{mgr: companionTestManager(t, &stubQuorum{events: make(chan meetings.Event, 1)})}
	if _, err := plain.StartCompanion(ctx, dashboard.CompanionStartRequest{ConsentVersion: meetings.CompanionConsentVersion, ConsentAttested: true}, "ana"); !errors.Is(err, dashboard.ErrMeetingUnavailable) {
		t.Fatalf("no companion quorum = %v", err)
	}
	if err := companionDashboardError(meetings.ErrNotCompanion); !errors.Is(err, dashboard.ErrMeetingInvalid) {
		t.Fatalf("not companion = %v", err)
	}
	if companionDashboardError(nil) != nil {
		t.Fatal("nil error")
	}
}

func companionTestManager(t *testing.T, q meetings.Quorum) *meetings.Manager {
	t.Helper()
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: q, ConsentText: "consent"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return mgr
}
