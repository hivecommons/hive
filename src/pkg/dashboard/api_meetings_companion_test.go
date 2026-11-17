package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCompanionService is the meetings fake plus the companion side.
type fakeCompanionService struct {
	fakeMeetingsService
	cmu       sync.Mutex
	cerr      error
	starts    []CompanionStartRequest
	startUser string
	audio     []string
	owner     bool
	after     int64
}

func (f *fakeCompanionService) CompanionInfo() any {
	return map[string]any{"consent_version": "companion-test", "sample_rate": 16000}
}

func (f *fakeCompanionService) StartCompanion(_ context.Context, req CompanionStartRequest, user string) (any, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.starts, f.startUser = append(f.starts, req), user
	if f.cerr != nil {
		return nil, f.cerr
	}
	return map[string]any{"id": "mtg-c1", "platform": "companion", "state": "live"}, nil
}

func (f *fakeCompanionService) CompanionAudio(_ context.Context, id, user string, asOwner bool, channel string, seq int, pcm []byte) (any, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.audio = append(f.audio, id+"/"+user+"/"+channel+"/"+string(rune('0'+seq))+"/"+string(pcm))
	f.owner = asOwner
	if f.cerr != nil {
		return nil, f.cerr
	}
	return map[string]any{"seq": seq, "duplicate": false}, nil
}

func (f *fakeCompanionService) CompanionReplies(_ context.Context, _, _ string, _ bool, after int64) (any, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.after = after
	if f.cerr != nil {
		return nil, f.cerr
	}
	return map[string]any{"lines": []any{}, "next_seq": after, "state": "live"}, nil
}

func (f *fakeCompanionService) EndCompanion(_ context.Context, id, _ string, _ bool) (any, error) {
	if f.cerr != nil {
		return nil, f.cerr
	}
	return map[string]any{"id": id, "state": "ended"}, nil
}

func companionRequest(s *Server, method, path, body, role string, verified bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Hive-User", "ana")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestCompanionRoutesNeedOwnerOrReadWriteAndTheFeature(t *testing.T) {
	s, _ := apiServer(t)
	if rec := companionRequest(s, http.MethodGet, "/api/companion/info", "", "read-write", false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no meetings = %d", rec.Code)
	}
	s.SetMeetingsService(&fakeMeetingsService{})
	if rec := companionRequest(s, http.MethodGet, "/api/companion/info", "", "read-write", false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("meetings without companion = %d", rec.Code)
	}
	s.SetMeetingsService(&fakeCompanionService{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/companion/info"},
		{http.MethodPost, "/api/companion/meetings"},
		{http.MethodPost, "/api/companion/meetings/mtg-c1/audio?channel=mic&seq=0"},
		{http.MethodGet, "/api/companion/meetings/mtg-c1/replies"},
		{http.MethodPost, "/api/companion/meetings/mtg-c1/end"},
	} {
		if rec := companionRequest(s, tc.method, tc.path, "{}", "read", false); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as read = %d", tc.method, tc.path, rec.Code)
		}
		if rec := companionRequest(s, tc.method, tc.path, "{}", "owner", false); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as unverified owner = %d", tc.method, tc.path, rec.Code)
		}
	}
	if rec := companionRequest(s, http.MethodGet, "/api/companion/info", "", "read-write", false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "companion-test") {
		t.Fatalf("info = %d %s", rec.Code, rec.Body.String())
	}
}

func TestCompanionStartAudioRepliesEnd(t *testing.T) {
	s, _ := apiServer(t)
	f := &fakeCompanionService{}
	s.SetMeetingsService(f)

	rec := companionRequest(s, http.MethodPost, "/api/companion/meetings",
		`{"title":"Standup <b>x</b>","app":"zoom","host_name":"Ana","consent_version":"v1","consent_attested":true}`, "read-write", false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	if len(f.starts) != 1 || f.starts[0].Title != "Standup x" || !f.starts[0].ConsentAttested || f.startUser != "ana" {
		t.Fatalf("start request = %+v by %q", f.starts, f.startUser)
	}
	if rec := companionRequest(s, http.MethodPost, "/api/companion/meetings", "not json", "read-write", false); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}

	if rec := companionRequest(s, http.MethodPost, "/api/companion/meetings/mtg-c1/audio?channel=mic&seq=3", "pcm", "owner", true); rec.Code != http.StatusOK {
		t.Fatalf("audio = %d %s", rec.Code, rec.Body.String())
	}
	if f.audio[0] != "mtg-c1/ana/mic/3/pcm" || !f.owner {
		t.Fatalf("audio call = %v owner=%v", f.audio, f.owner)
	}
	for _, q := range []string{"channel=mic", "channel=mic&seq=-1", "channel=mic&seq=x"} {
		if rec := companionRequest(s, http.MethodPost, "/api/companion/meetings/mtg-c1/audio?"+q, "", "read-write", false); rec.Code != http.StatusBadRequest {
			t.Errorf("audio %q = %d", q, rec.Code)
		}
	}
	big := strings.Repeat("a", companionMaxChunkBytes+1)
	if rec := companionRequest(s, http.MethodPost, "/api/companion/meetings/mtg-c1/audio?channel=mic&seq=0", big, "read-write", false); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big chunk = %d", rec.Code)
	}

	if rec := companionRequest(s, http.MethodGet, "/api/companion/meetings/mtg-c1/replies?after=7", "", "read-write", false); rec.Code != http.StatusOK || f.after != 7 {
		t.Fatalf("replies = %d after=%d", rec.Code, f.after)
	}
	if rec := companionRequest(s, http.MethodGet, "/api/companion/meetings/mtg-c1/replies?after=-2", "", "read-write", false); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad after = %d", rec.Code)
	}
	if rec := companionRequest(s, http.MethodPost, "/api/companion/meetings/mtg-c1/end", "", "read-write", false); rec.Code != http.StatusOK {
		t.Fatalf("end = %d", rec.Code)
	}
}

func TestCompanionErrorsMapToStatuses(t *testing.T) {
	s, _ := apiServer(t)
	f := &fakeCompanionService{}
	s.SetMeetingsService(f)
	for _, tc := range []struct {
		err  error
		code int
	}{
		{&CompanionGapError{Expected: 4}, http.StatusConflict},
		{ErrMeetingForbidden, http.StatusForbidden},
		{ErrMeetingUnavailable, http.StatusServiceUnavailable},
		{ErrMeetingNotFound, http.StatusNotFound},
		{ErrMeetingInvalid, http.StatusBadRequest},
		{ErrMeetingConflict, http.StatusConflict},
		{errors.New("quorum down"), http.StatusBadGateway},
	} {
		f.cerr = tc.err
		for _, call := range []struct{ method, path string }{
			{http.MethodPost, "/api/companion/meetings"},
			{http.MethodPost, "/api/companion/meetings/mtg-c1/audio?channel=call&seq=1"},
			{http.MethodGet, "/api/companion/meetings/mtg-c1/replies"},
			{http.MethodPost, "/api/companion/meetings/mtg-c1/end"},
		} {
			body := "{}"
			rec := companionRequest(s, call.method, call.path, body, "read-write", false)
			if rec.Code != tc.code {
				t.Errorf("%v on %s = %d, want %d", tc.err, call.path, rec.Code, tc.code)
			}
		}
	}
	f.cerr = &CompanionGapError{Expected: 4}
	rec := companionRequest(s, http.MethodPost, "/api/companion/meetings/mtg-c1/audio?channel=call&seq=9", "", "read-write", false)
	var body struct {
		ExpectedSeq int `json:"expected_seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.ExpectedSeq != 4 {
		t.Fatalf("gap body = %s (%v)", rec.Body.String(), err)
	}
}
