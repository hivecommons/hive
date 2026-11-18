package dashboard

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Meeting schedule API (hivecommons/hive#18394): the Calendar section's
// scheduled meetings, manual adds and approvals. pkg/dashboard never
// imports pkg/meetings; the scheduler arrives through
// MeetingsScheduleService, which cmd/hive implements.
//
// The routes sit under /api/meetings/schedule. GET /api/meetings/schedule
// is a literal and wins over GET /api/meetings/{id}; the action routes have
// four segments, so they never match a /api/meetings/{id}/... route.

// MeetingScheduleAddRequest is POST /api/meetings/schedule: a meeting link,
// its start (RFC 3339), and optionally an end, a title and a recurrence
// rule (RFC 5545 RRULE, expanded in timezone, an IANA zone name).
type MeetingScheduleAddRequest struct {
	URL      string `json:"url"`
	Start    string `json:"start"`
	End      string `json:"end,omitempty"`
	Title    string `json:"title,omitempty"`
	RRule    string `json:"rrule,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

// MeetingScheduleAdd is a validated manual add.
type MeetingScheduleAdd struct {
	URL      string
	Title    string
	Start    time.Time
	End      time.Time
	RRule    string
	Timezone string
	// AddedBy is the dashboard user; ByOwner is true for the hive owner,
	// whose own adds are scheduled without approval.
	AddedBy string
	ByOwner bool
}

// Meeting schedule actions (POST /api/meetings/schedule/{eid}/{action}).
const (
	MeetingScheduleApprove = "approve"
	MeetingScheduleDecline = "decline"
	MeetingScheduleCancel  = "cancel"
	MeetingScheduleSkip    = "skip"
)

// MeetingsScheduleService is the meeting scheduler as the dashboard sees
// it. Values are JSON-encoded as returned; errors wrap the ErrMeeting*
// sentinels.
type MeetingsScheduleService interface {
	// List returns {upcoming, recent, attendee_name}.
	List() (any, error)
	Add(ctx context.Context, req MeetingScheduleAdd) (any, error)
	// Act runs approve, decline, cancel or skip on one event.
	Act(ctx context.Context, eid, action, by string) (any, error)
	// Delete removes a finished event and the finished meetings it
	// created (ErrMeetingConflict for an event still to come or live).
	Delete(ctx context.Context, eid string) (MeetingScheduleDeleted, error)
}

type meetingsScheduleHolder struct{ svc MeetingsScheduleService }

// SetMeetingsScheduleService installs the scheduler (nil removes it).
func (s *Server) SetMeetingsScheduleService(svc MeetingsScheduleService) {
	s.meetingsSchedule.Store(&meetingsScheduleHolder{svc: svc})
}

func (s *Server) meetingsScheduleService(w http.ResponseWriter) (MeetingsScheduleService, bool) {
	h := s.meetingsSchedule.Load()
	if h == nil || h.svc == nil {
		jsonError(w, "meetings are not enabled on this hive (Settings, Meetings)", http.StatusServiceUnavailable)
		return nil, false
	}
	return h.svc, true
}

func (s *Server) registerMeetingsScheduleRoutes() {
	s.mux.HandleFunc("GET /api/meetings/schedule", s.handleMeetingsScheduleList)
	s.mux.HandleFunc("POST /api/meetings/schedule", s.handleMeetingsScheduleAdd)
	s.mux.HandleFunc("POST /api/meetings/schedule/{eid}/approve", s.meetingsScheduleAction(MeetingScheduleApprove))
	s.mux.HandleFunc("POST /api/meetings/schedule/{eid}/decline", s.meetingsScheduleAction(MeetingScheduleDecline))
	s.mux.HandleFunc("POST /api/meetings/schedule/{eid}/cancel", s.meetingsScheduleAction(MeetingScheduleCancel))
	s.mux.HandleFunc("POST /api/meetings/schedule/{eid}/skip", s.meetingsScheduleAction(MeetingScheduleSkip))
	// Drafts from meetings (#18406) share the Calendar's approvals.
	s.registerMeetingsActionsRoutes()
}

// requireMeetingsScheduleRole admits the verified owner and read-write
// users: they may see the calendar and propose a meeting; a read-write
// user's add waits for the owner's approval.
func requireMeetingsScheduleRole(w http.ResponseWriter, r *http.Request) (byOwner, ok bool) {
	role := r.Header.Get("X-Hive-Role")
	if isOwnerRole(role) {
		if r.Header.Get(ownerRoleVerifiedHeader) != "true" {
			jsonError(w, "owner or read-write access required", http.StatusForbidden)
			return false, false
		}
		return true, true
	}
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		jsonError(w, "owner or read-write access required", http.StatusForbidden)
		return false, false
	}
	return false, true
}

func (s *Server) handleMeetingsScheduleList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireMeetingsScheduleRole(w, r); !ok {
		return
	}
	svc, ok := s.meetingsScheduleService(w)
	if !ok {
		return
	}
	out, err := svc.List()
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	jsonResponse(w, s.withEmailIn(r, out))
}

// parseMeetingScheduleAdd validates the request shape; the scheduler
// checks the link, recurrence and timezone.
func parseMeetingScheduleAdd(req MeetingScheduleAddRequest) (MeetingScheduleAdd, string) {
	out := MeetingScheduleAdd{
		URL: strings.TrimSpace(req.URL), Title: strings.TrimSpace(req.Title),
		RRule: strings.TrimSpace(req.RRule), Timezone: strings.TrimSpace(req.Timezone),
	}
	if out.URL == "" {
		return out, "url is required"
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Start))
	if err != nil {
		return out, "start must be an RFC 3339 time"
	}
	out.Start = start
	if e := strings.TrimSpace(req.End); e != "" {
		end, err := time.Parse(time.RFC3339, e)
		if err != nil {
			return out, "end must be an RFC 3339 time"
		}
		out.End = end
	}
	return out, ""
}

func (s *Server) handleMeetingsScheduleAdd(w http.ResponseWriter, r *http.Request) {
	byOwner, ok := requireMeetingsScheduleRole(w, r)
	if !ok {
		return
	}
	svc, ok := s.meetingsScheduleService(w)
	if !ok {
		return
	}
	var req MeetingScheduleAddRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	add, msg := parseMeetingScheduleAdd(req)
	if msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	add.AddedBy, add.ByOwner = strings.TrimSpace(r.Header.Get("X-Hive-User")), byOwner
	ev, err := svc.Add(r.Context(), add)
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	s.auditFromRequest(r, "meeting_schedule_add", auditDetail("by_owner", boolString(byOwner), "recurring", boolString(add.RRule != "")), "")
	jsonResponse(w, ev)
}

// meetingsScheduleAction answers approve, decline, cancel and skip; each is
// the owner's call.
func (s *Server) meetingsScheduleAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireOwnerRole(w, r) {
			return
		}
		svc, ok := s.meetingsScheduleService(w)
		if !ok {
			return
		}
		eid := r.PathValue("eid")
		ev, err := svc.Act(r.Context(), eid, action, strings.TrimSpace(r.Header.Get("X-Hive-User")))
		if err != nil {
			s.meetingsError(w, r, err)
			return
		}
		s.auditFromRequest(r, "meeting_schedule_"+action, auditDetail("event", eid), "")
		jsonResponse(w, ev)
	}
}
