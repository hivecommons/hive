package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeScheduleService struct {
	err    error
	added  []MeetingScheduleAdd
	acted  []string
	listed int
	// deleted are the Delete calls; deleteRes is returned for them.
	deleted   []string
	deleteRes MeetingScheduleDeleted
}

func (f *fakeScheduleService) List() (any, error) {
	f.listed++
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"upcoming": []any{map[string]string{"id": "e1"}}, "recent": []any{}, "attendee_name": "Betty"}, nil
}

func (f *fakeScheduleService) Add(_ context.Context, req MeetingScheduleAdd) (any, error) {
	f.added = append(f.added, req)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"id": "e2", "status": "scheduled"}, nil
}

func (f *fakeScheduleService) Act(_ context.Context, eid, action, by string) (any, error) {
	f.acted = append(f.acted, eid+":"+action+":"+by)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"id": eid, "status": action}, nil
}

func schedulePost(s *Server, path, body, role string, verified bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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

var scheduleActionPaths = []string{
	"/api/meetings/schedule/e1/approve", "/api/meetings/schedule/e1/decline",
	"/api/meetings/schedule/e1/cancel", "/api/meetings/schedule/e1/skip",
}

func TestMeetingsScheduleAPIAuthAndUnavailable(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doOwnerGet(s, "/api/meetings/schedule"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no service = %d", rec.Code)
	}
	if rec := schedulePost(s, "/api/meetings/schedule", `{"url":"https://meet.google.com/x","start":"2026-11-16T15:00:00Z"}`, "owner", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("add without service = %d", rec.Code)
	}
	for _, p := range scheduleActionPaths {
		if rec := schedulePost(s, p, ``, "owner", true); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without service = %d", p, rec.Code)
		}
	}
	s.SetMeetingsScheduleService(&fakeScheduleService{})
	// Read-only and anonymous viewers never see the calendar.
	for _, role := range []string{"", "read"} {
		if rec := doGetRole(s, "/api/meetings/schedule", role, false); rec.Code != http.StatusForbidden {
			t.Errorf("GET as %q = %d, want 403", role, rec.Code)
		}
		if rec := schedulePost(s, "/api/meetings/schedule", `{}`, role, false); rec.Code != http.StatusForbidden {
			t.Errorf("POST as %q = %d, want 403", role, rec.Code)
		}
	}
	// An unverified owner header is refused.
	if rec := doGetRole(s, "/api/meetings/schedule", "owner", false); rec.Code != http.StatusForbidden {
		t.Errorf("unverified owner GET = %d", rec.Code)
	}
	if rec := schedulePost(s, "/api/meetings/schedule", `{}`, "owner", false); rec.Code != http.StatusForbidden {
		t.Errorf("unverified owner POST = %d", rec.Code)
	}
	// Approve, decline, cancel and skip are the owner's alone.
	for _, p := range scheduleActionPaths {
		if rec := schedulePost(s, p, ``, "read-write", false); rec.Code != http.StatusForbidden {
			t.Errorf("%s as read-write = %d, want 403", p, rec.Code)
		}
		if rec := schedulePost(s, p, ``, "owner", false); rec.Code != http.StatusForbidden {
			t.Errorf("%s unverified = %d, want 403", p, rec.Code)
		}
	}
	if rec := doGetRole(s, "/api/meetings/schedule", "read-write", false); rec.Code != http.StatusOK {
		t.Errorf("read-write GET = %d", rec.Code)
	}
}

func TestMeetingsScheduleAPIRoutes(t *testing.T) {
	s, _ := apiServer(t)
	f := &fakeScheduleService{}
	s.SetMeetingsScheduleService(f)
	// The schedule routes do not collide with /api/meetings/{id} routes:
	// with no meetings service installed those answer 503, these 200.
	rec := doOwnerGet(s, "/api/meetings/schedule")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"attendee_name":"Betty"`) || f.listed != 1 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	body := `{"url":" https://meet.google.com/x ","start":"2026-11-16T15:00:00Z","end":"2026-11-16T16:00:00-05:00","title":" Sync ","rrule":"FREQ=WEEKLY","timezone":"America/New_York"}`
	rec = schedulePost(s, "/api/meetings/schedule", body, "owner", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "e2") {
		t.Fatalf("owner add = %d %s", rec.Code, rec.Body.String())
	}
	a := f.added[0]
	if a.URL != "https://meet.google.com/x" || a.Title != "Sync" || a.RRule != "FREQ=WEEKLY" || a.Timezone != "America/New_York" ||
		!a.ByOwner || a.AddedBy != "octo" || a.Start.Hour() != 15 || a.End.UTC().Hour() != 21 {
		t.Fatalf("parsed add = %+v", a)
	}
	rec = schedulePost(s, "/api/meetings/schedule", `{"url":"https://meet.google.com/x","start":"2026-11-16T15:00:00Z"}`, "read-write", false)
	if rec.Code != http.StatusOK || f.added[1].ByOwner {
		t.Fatalf("read-write add = %d %+v", rec.Code, f.added[1])
	}
	for _, bad := range []string{`{`, `{"start":"2026-11-16T15:00:00Z"}`, `{"url":"https://meet.google.com/x","start":"tomorrow"}`,
		`{"url":"https://meet.google.com/x","start":"2026-11-16T15:00:00Z","end":"later"}`} {
		if rec := schedulePost(s, "/api/meetings/schedule", bad, "owner", true); rec.Code != http.StatusBadRequest {
			t.Errorf("bad add %s = %d", bad, rec.Code)
		}
	}
	for _, p := range scheduleActionPaths {
		if rec := schedulePost(s, p, ``, "owner", true); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", p, rec.Code, rec.Body.String())
		}
	}
	want := []string{"e1:approve:octo", "e1:decline:octo", "e1:cancel:octo", "e1:skip:octo"}
	if strings.Join(f.acted, ",") != strings.Join(want, ",") {
		t.Fatalf("acted = %v", f.acted)
	}
	// An unknown action is not routed at all.
	if rec := schedulePost(s, "/api/meetings/schedule/e1/explode", ``, "owner", true); rec.Code == http.StatusOK {
		t.Fatalf("unknown action routed: %d", rec.Code)
	}

	cases := map[error]int{
		fmt.Errorf("%w: x", ErrMeetingNotFound): http.StatusNotFound,
		fmt.Errorf("%w: x", ErrMeetingInvalid):  http.StatusBadRequest,
		fmt.Errorf("%w: x", ErrMeetingConflict): http.StatusConflict,
		errors.New("disk"):                      http.StatusBadGateway,
	}
	for err, code := range cases {
		f.err = err
		if rec := doOwnerGet(s, "/api/meetings/schedule"); rec.Code != code {
			t.Errorf("list %v = %d, want %d", err, rec.Code, code)
		}
		if rec := schedulePost(s, "/api/meetings/schedule", `{"url":"https://meet.google.com/x","start":"2026-11-16T15:00:00Z"}`, "owner", true); rec.Code != code {
			t.Errorf("add %v = %d, want %d", err, rec.Code, code)
		}
		if rec := schedulePost(s, scheduleActionPaths[0], ``, "owner", true); rec.Code != code {
			t.Errorf("act %v = %d, want %d", err, rec.Code, code)
		}
	}
	s.SetMeetingsScheduleService(nil)
	if rec := doOwnerGet(s, "/api/meetings/schedule"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("removed service = %d", rec.Code)
	}
}
