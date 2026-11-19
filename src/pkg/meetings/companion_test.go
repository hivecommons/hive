package meetings

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeCompanionQuorum adds the companion routes to fakeQuorum.
type fakeCompanionQuorum struct {
	*fakeQuorum
	cmu      sync.Mutex
	chunks   []string
	audioErr error
	lines    []CompanionLine
	tone     bool
}

func (f *fakeCompanionQuorum) CompanionAudio(_ context.Context, nativeID, channel string, seq int, pcm []byte) (CompanionChunk, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	if f.audioErr != nil {
		return CompanionChunk{}, f.audioErr
	}
	f.chunks = append(f.chunks, nativeID+"/"+channel)
	return CompanionChunk{Seq: seq}, nil
}

func (f *fakeCompanionQuorum) CompanionOutbox(_ context.Context, _ string, after int64) (CompanionPage, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	out := CompanionPage{NextSeq: after, Tone: f.tone}
	f.tone = false
	for _, l := range f.lines {
		if l.Seq > after {
			out.Lines = append(out.Lines, l)
			out.NextSeq = l.Seq
		}
	}
	return out, nil
}

func newCompanionFake() *fakeCompanionQuorum {
	q := newFakeQuorum()
	q.joinRes = JoinResult{MeetingID: "qc1", Platform: PlatformCompanion, State: StateLive}
	return &fakeCompanionQuorum{fakeQuorum: q}
}

func validStart() CompanionStart {
	return CompanionStart{Title: "Standup", App: "Zoom", ConsentVersion: CompanionConsentVersion, Attested: true, User: "ana"}
}

func waitConsent(t *testing.T, m *Manager, id string) Meeting {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mt, err := m.Get(id)
		if err == nil && mt.ConsentPostedAt != nil {
			return mt
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("consent notice never posted")
	return Meeting{}
}

func TestStartCompanionRecordsConsentAndPostsTheNoticeToTheOutbox(t *testing.T) {
	q := newCompanionFake()
	m := newTestManager(t, q, time.Minute)
	mt, err := m.StartCompanion(context.Background(), validStart())
	if err != nil {
		t.Fatal(err)
	}
	if mt.Platform != PlatformCompanion || mt.State != StateLive || mt.NativeID != "qc1" || mt.InvitedBy != "ana" {
		t.Fatalf("meeting = %+v", mt)
	}
	if mt.Companion == nil || mt.Companion.App != "zoom" || mt.Companion.ConsentVersion != CompanionConsentVersion || mt.Companion.AttestedBy != "ana" {
		t.Fatalf("companion info = %+v", mt.Companion)
	}
	q.mu.Lock()
	join := q.joins[0]
	q.mu.Unlock()
	if join.Platform != PlatformCompanion || join.HostName != "ana" || join.URL != "" || join.Voice {
		t.Fatalf("join args = %+v", join)
	}
	q.stream("qc1") <- Event{Type: EventState, MeetingID: "qc1", State: StateLive}
	waitConsent(t, m, mt.ID)
	if posts, _ := q.snapshot(); len(posts) == 0 || posts[0] != testConsent {
		t.Fatalf("posts = %v", posts)
	}
}

func TestStartCompanionRefusals(t *testing.T) {
	m := newTestManager(t, newCompanionFake(), time.Minute)
	ctx := context.Background()
	for name, mut := range map[string]func(*CompanionStart){
		"not attested":  func(r *CompanionStart) { r.Attested = false },
		"old notice":    func(r *CompanionStart) { r.ConsentVersion = "companion-1999" },
		"no user":       func(r *CompanionStart) { r.User = " " },
		"long title":    func(r *CompanionStart) { r.Title = string(make([]byte, companionTitleMax+1)) },
		"long app name": func(r *CompanionStart) { r.App = string(make([]byte, companionNameMax+1)) },
	} {
		req := validStart()
		mut(&req)
		if _, err := m.StartCompanion(ctx, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	plain := newTestManager(t, newFakeQuorum(), time.Minute)
	if _, err := plain.StartCompanion(ctx, validStart()); !errors.Is(err, ErrCompanionUnavailable) {
		t.Fatalf("no companion quorum: %v", err)
	}
	q := newCompanionFake()
	q.joinErr = &ToolError{Tool: "meeting.join", Code: CodePlatformUnavailable, Message: "no transcription"}
	failing := newTestManager(t, q, time.Minute)
	mt, err := failing.StartCompanion(ctx, validStart())
	if !IsCode(err, CodePlatformUnavailable) || mt.State != StateFailed {
		t.Fatalf("refused join = %+v, %v", mt, err)
	}
}

func TestCompanionAudioRepliesAndEnd(t *testing.T) {
	q := newCompanionFake()
	m := newTestManager(t, q, time.Minute)
	ctx := context.Background()
	mt, err := m.StartCompanion(ctx, validStart())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompanionAudio(ctx, mt.ID, "ana", false, CompanionChannelCall, 0, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompanionAudio(ctx, mt.ID, "bob", false, CompanionChannelMic, 0, nil); !errors.Is(err, ErrCompanionNotYours) {
		t.Fatalf("other user = %v", err)
	}
	if _, err := m.CompanionAudio(ctx, mt.ID, "owner", true, CompanionChannelMic, 0, nil); err != nil {
		t.Fatalf("owner = %v", err)
	}
	for name, call := range map[string]func() error{
		"bad channel": func() error { _, err := m.CompanionAudio(ctx, mt.ID, "ana", false, "video", 0, nil); return err },
		"negative": func() error {
			_, err := m.CompanionAudio(ctx, mt.ID, "ana", false, CompanionChannelMic, -1, nil)
			return err
		},
		"odd bytes": func() error {
			_, err := m.CompanionAudio(ctx, mt.ID, "ana", false, CompanionChannelMic, 1, []byte{1})
			return err
		},
		"too big": func() error {
			_, err := m.CompanionAudio(ctx, mt.ID, "ana", false, CompanionChannelMic, 1, make([]byte, CompanionMaxChunkBytes+2))
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	q.chunks = nil
	q.audioErr = &ChunkGapError{Expected: 3}
	var gap *ChunkGapError
	if _, err := m.CompanionAudio(ctx, mt.ID, "ana", false, CompanionChannelCall, 5, nil); !errors.As(err, &gap) || gap.Expected != 3 {
		t.Fatalf("gap = %v", err)
	}

	q.lines = []CompanionLine{{Seq: 1, Text: "consent"}, {Seq: 2, Text: "an answer"}}
	r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 1)
	if err != nil || len(r.Lines) != 1 || r.Lines[0].Text != "an answer" || r.NextSeq != 2 || r.State != StateLive {
		t.Fatalf("replies = %+v, %v", r, err)
	}
	if r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 9); err != nil || r.Lines == nil || len(r.Lines) != 0 || r.Tone {
		t.Fatalf("empty replies = %+v, %v", r, err)
	}
	// A listening tone Quorum holds for the window is passed through once.
	q.cmu.Lock()
	q.tone = true
	q.cmu.Unlock()
	if r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 2); err != nil || !r.Tone || len(r.Lines) != 0 {
		t.Fatalf("replies with a tone = %+v, %v", r, err)
	}
	if r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 2); err != nil || r.Tone {
		t.Fatalf("the tone is delivered once = %+v, %v", r, err)
	}

	ended, err := m.EndCompanion(ctx, mt.ID, "ana", false)
	if err != nil || ended.State != StateEnded || ended.StateReason != ReasonCompanionEnded {
		t.Fatalf("end = %+v, %v", ended, err)
	}
	// The lines Quorum still held are served once after the end (companion_tail.go).
	if r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 0); err != nil || r.State != StateEnded || len(r.Lines) != 2 || r.NextSeq != 2 {
		t.Fatalf("replies after end = %+v, %v", r, err)
	}
	if r, err := m.CompanionReplies(ctx, mt.ID, "ana", false, 2); err != nil || r.State != StateEnded || len(r.Lines) != 0 {
		t.Fatalf("replies after the last lines were fetched = %+v, %v", r, err)
	}
	if _, err := m.CompanionAudio(ctx, mt.ID, "ana", false, CompanionChannelCall, 0, nil); !errors.Is(err, ErrNotLive) {
		t.Fatalf("audio after end = %v", err)
	}
	if _, err := m.EndCompanion(ctx, "mtg-missing", "ana", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestCompanionRoutesRefuseOtherMeetings(t *testing.T) {
	q := newCompanionFake()
	q.joinRes = JoinResult{MeetingID: "q-meet", Platform: PlatformGoogleMeet, State: StateJoining}
	m := newTestManager(t, q, time.Minute)
	mt, err := m.Join(context.Background(), JoinRequest{URL: "https://meet.google.com/abc-defg-hij"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompanionReplies(context.Background(), mt.ID, "ana", true, 0); !errors.Is(err, ErrNotCompanion) {
		t.Fatalf("meet meeting = %v", err)
	}
	if _, err := m.Join(context.Background(), JoinRequest{Platform: PlatformCompanion}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("companion through Join = %v", err)
	}
}

func TestClientCompanionRoutes(t *testing.T) {
	var gotAuth, gotQuery string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("meeting_id") {
		case "gap":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"out of order","expected_seq":4}`)
		case "ended":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"not live","code":"not_live"}`)
		case "missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no meeting"}`)
		case "bad":
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"error":"too large"}`)
		case "boom":
			w.WriteHeader(http.StatusInternalServerError)
		case "garbled":
			_, _ = io.WriteString(w, `not json`)
		default:
			if r.URL.Path == "/companion/outbox" {
				_, _ = io.WriteString(w, `{"lines":[{"seq":3,"text":"hi","at":"2026-11-16T12:00:00Z"}],"next_seq":3,"tone":true}`)
				return
			}
			_, _ = io.WriteString(w, `{"seq":2,"duplicate":true}`)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	ctx := context.Background()

	res, err := c.CompanionAudio(ctx, "m1", CompanionChannelMic, 2, []byte{1, 2})
	if err != nil || res.Seq != 2 || !res.Duplicate {
		t.Fatalf("audio = %+v, %v", res, err)
	}
	if gotAuth != "Bearer tok" || gotQuery != "channel=mic&meeting_id=m1&seq=2" || string(gotBody) != "\x01\x02" {
		t.Fatalf("request: auth=%q query=%q body=%v", gotAuth, gotQuery, gotBody)
	}
	page, err := c.CompanionOutbox(ctx, "m1", -5)
	if err != nil || page.NextSeq != 3 || len(page.Lines) != 1 || page.Lines[0].Text != "hi" || !page.Tone || gotQuery != "after=0&meeting_id=m1" {
		t.Fatalf("outbox = %+v, %v (query %q)", page, err, gotQuery)
	}

	var gap *ChunkGapError
	if _, err := c.CompanionAudio(ctx, "gap", CompanionChannelCall, 9, nil); !errors.As(err, &gap) || gap.Expected != 4 || gap.Error() == "" {
		t.Fatalf("gap = %v", err)
	}
	if _, err := c.CompanionAudio(ctx, "ended", CompanionChannelCall, 0, nil); !errors.Is(err, ErrNotLive) {
		t.Fatalf("ended = %v", err)
	}
	if _, err := c.CompanionOutbox(ctx, "missing", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := c.CompanionAudio(ctx, "bad", CompanionChannelCall, 0, nil); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("too large = %v", err)
	}
	if _, err := c.CompanionAudio(ctx, "boom", CompanionChannelCall, 0, nil); err == nil {
		t.Fatal("500 should fail")
	}
	if _, err := c.CompanionAudio(ctx, "garbled", CompanionChannelCall, 0, nil); err == nil {
		t.Fatal("garbled audio reply should fail")
	}
	if _, err := c.CompanionOutbox(ctx, "garbled", 0); err == nil {
		t.Fatal("garbled outbox reply should fail")
	}
	srv.Close()
	if _, err := c.CompanionAudio(ctx, "m1", CompanionChannelCall, 0, nil); err == nil {
		t.Fatal("closed server should fail")
	}
}

func TestIsCompanionChannel(t *testing.T) {
	if !IsCompanionChannel("call") || !IsCompanionChannel("mic") || IsCompanionChannel("") {
		t.Fatal("channel check")
	}
}
