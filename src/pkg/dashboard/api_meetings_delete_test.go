package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func (f *fakeMeetingsService) Delete(_ context.Context, id string) (MeetingDeleted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	if f.err != nil {
		return MeetingDeleted{}, f.err
	}
	d := f.deleteRes
	d.ID = id
	return d, nil
}

func (f *fakeMeetingsService) ClearFinished(_ context.Context, before time.Time) ([]MeetingDeleted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, before)
	if f.err != nil {
		return nil, f.err
	}
	return []MeetingDeleted{{ID: "mtg-a", State: "ended"}, {ID: "mtg-b", State: "blocked"}}, nil
}

func (f *fakeScheduleService) Delete(_ context.Context, eid string) (MeetingScheduleDeleted, error) {
	f.deleted = append(f.deleted, eid)
	if f.err != nil {
		return MeetingScheduleDeleted{}, f.err
	}
	d := f.deleteRes
	d.ID = eid
	return d, nil
}

func meetingsDeleteAs(s *Server, method, path, body, role string, verified bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Hive-User", "octo")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

var meetingsDeleteRoutes = []struct{ method, path string }{
	{http.MethodDelete, "/api/meetings/mtg-1"},
	{http.MethodPost, "/api/meetings/clear-finished"},
	{http.MethodDelete, "/api/meetings/schedule/e1"},
}

func TestMeetingsDeleteRoleMatrix(t *testing.T) {
	s, _ := apiServer(t)
	for _, c := range meetingsDeleteRoutes {
		if rec := meetingsDeleteAs(s, c.method, c.path, "", "owner", true); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without service = %d, want 503", c.method, c.path, rec.Code)
		}
	}
	f := &fakeMeetingsService{}
	sf := &fakeScheduleService{}
	s.SetMeetingsService(f)
	s.SetMeetingsScheduleService(sf)
	for _, c := range meetingsDeleteRoutes {
		for _, who := range []struct {
			role     string
			verified bool
		}{{"read-write", true}, {"read", true}, {"", false}, {"owner", false}} {
			if rec := meetingsDeleteAs(s, c.method, c.path, "", who.role, who.verified); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %q (verified=%v) = %d, want 403", c.method, c.path, who.role, who.verified, rec.Code)
			}
		}
	}
	if len(f.deleted) != 0 || len(f.cleared) != 0 || len(sf.deleted) != 0 {
		t.Fatalf("a refused request reached the service: %v %v %v", f.deleted, f.cleared, sf.deleted)
	}
	for _, c := range meetingsDeleteRoutes {
		if rec := meetingsDeleteAs(s, c.method, c.path, "", "owner", true); rec.Code != http.StatusOK {
			t.Errorf("owner %s %s = %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	if len(f.deleted) != 1 || f.deleted[0] != "mtg-1" || len(f.cleared) != 1 || len(sf.deleted) != 1 || sf.deleted[0] != "e1" {
		t.Fatalf("owner calls: %v %v %v", f.deleted, f.cleared, sf.deleted)
	}
}

func TestMeetingsDeleteStatesAndAudit(t *testing.T) {
	s, _ := apiServer(t)
	f := &fakeMeetingsService{deleteRes: MeetingDeleted{State: "failed", UpstreamError: "quorum meeting.delete: upstream_error: vexa 500", CalendarRemoved: 1}}
	s.SetMeetingsService(f)
	rec := meetingsDeleteAs(s, http.MethodDelete, "/api/meetings/mtg-1", "", "owner", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	var got MeetingDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != "mtg-1" || got.UpstreamError == "" || got.CalendarRemoved != 1 {
		t.Fatalf("delete body = %s (%v)", rec.Body.String(), err)
	}
	e := s.audit.Recent(1)
	if len(e) != 1 || e[0].Action != "meeting_delete" || !strings.Contains(e[0].Detail, "meeting=mtg-1") ||
		!strings.Contains(e[0].Detail, "upstream_error=true") || !strings.Contains(e[0].Detail, "calendar_removed=1") {
		t.Fatalf("delete not audited: %+v", e)
	}
	// A meeting hive is still in answers 409; unknown 404. Neither is audited.
	for err, want := range map[error]int{
		fmt.Errorf("%w: leave the meeting first", ErrMeetingConflict): http.StatusConflict,
		fmt.Errorf("%w: mtg-x", ErrMeetingNotFound):                   http.StatusNotFound,
	} {
		f.err = err
		before := len(s.audit.Recent(100))
		if rec := meetingsDeleteAs(s, http.MethodDelete, "/api/meetings/mtg-1", "", "owner", true); rec.Code != want {
			t.Errorf("delete with %v = %d, want %d", err, rec.Code, want)
		}
		if len(s.audit.Recent(100)) != before {
			t.Errorf("a refused delete (%v) was audited", err)
		}
	}
	f.err = nil
}

func TestMeetingsClearFinished(t *testing.T) {
	s, _ := apiServer(t)
	f := &fakeMeetingsService{}
	s.SetMeetingsService(f)
	cut := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	rec := meetingsDeleteAs(s, http.MethodPost, "/api/meetings/clear-finished", `{"before":"`+cut.Format(time.RFC3339)+`"}`, "owner", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"count":2`) {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body.String())
	}
	if !f.cleared[0].Equal(cut) {
		t.Fatalf("cutoff = %v, want %v", f.cleared[0], cut)
	}
	if e := s.audit.Recent(2); len(e) != 2 || e[0].Action != "meeting_delete" || e[1].Action != "meeting_delete" {
		t.Fatalf("each cleared meeting is audited: %+v", e)
	}
	// A cutoff in the future is clamped to now: only meetings older than now go.
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if rec := meetingsDeleteAs(s, http.MethodPost, "/api/meetings/clear-finished", `{"before":"`+future+`"}`, "owner", true); rec.Code != http.StatusOK {
		t.Fatalf("future clear = %d", rec.Code)
	}
	if f.cleared[1].After(time.Now()) {
		t.Fatalf("future cutoff not clamped: %v", f.cleared[1])
	}
	if rec := meetingsDeleteAs(s, http.MethodPost, "/api/meetings/clear-finished", `{"before":"yesterday"}`, "owner", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cutoff = %d", rec.Code)
	}
}

func TestMeetingsScheduleDelete(t *testing.T) {
	s, _ := apiServer(t)
	sf := &fakeScheduleService{deleteRes: MeetingScheduleDeleted{Meetings: []MeetingDeleted{{ID: "mtg-1", State: "ended"}}}}
	s.SetMeetingsScheduleService(sf)
	rec := meetingsDeleteAs(s, http.MethodDelete, "/api/meetings/schedule/e1", "", "owner", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mtg-1"`) {
		t.Fatalf("calendar delete = %d %s", rec.Code, rec.Body.String())
	}
	e := s.audit.Recent(2)
	// Newest first: the entry, after the meeting it created.
	if len(e) != 2 || e[0].Action != "meeting_schedule_delete" || !strings.Contains(e[0].Detail, "event=e1") ||
		e[1].Action != "meeting_delete" || !strings.Contains(e[1].Detail, "via=calendar") {
		t.Fatalf("calendar delete audit: %+v", e)
	}
	sf.err = fmt.Errorf("%w: e1 is scheduled; skip or cancel it instead", ErrMeetingConflict)
	if rec := meetingsDeleteAs(s, http.MethodDelete, "/api/meetings/schedule/e1", "", "owner", true); rec.Code != http.StatusConflict {
		t.Fatalf("future entry delete = %d, want 409", rec.Code)
	}
}
