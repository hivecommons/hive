package dashboard

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Deleting meetings (owner only): the Meetings section's 🗑 Delete and
// "Clear finished", and 🗑 Delete on past Calendar entries. A meeting in a
// final state (ended, failed, blocked, declined, left, handed off, purged)
// or a pending platform join request may be deleted; one hive is still
// joining, waiting in the lobby of or live in answers 409 (leave it
// first). Delete removes the meeting record, its transcript and live state
// on this hive, erases it in Vexa (Quorum meeting.delete, best effort),
// and drops the Calendar entry that existed only for it. Knowledge facts
// and drafts already made from the meeting are not touched.

// MeetingDeleted is one deleted meeting. UpstreamError is set when Vexa
// could not confirm the erase; the record on this hive is gone anyway.
type MeetingDeleted struct {
	ID              string `json:"id"`
	State           string `json:"state,omitempty"`
	UpstreamError   string `json:"upstream_error,omitempty"`
	CalendarRemoved int    `json:"calendar_removed,omitempty"`
}

// MeetingScheduleDeleted is a deleted Calendar entry and the meetings it
// created that went with it.
type MeetingScheduleDeleted struct {
	ID       string           `json:"id"`
	Meetings []MeetingDeleted `json:"meetings"`
}

// meetingsClearRequest is POST /api/meetings/clear-finished: meetings that
// finished before `before` (RFC 3339; empty means now) are deleted.
type meetingsClearRequest struct {
	Before string `json:"before,omitempty"`
}

func (s *Server) registerMeetingsDeleteRoutes() {
	s.mux.HandleFunc("DELETE /api/meetings/{id}", s.handleMeetingsDelete)
	s.mux.HandleFunc("POST /api/meetings/clear-finished", s.handleMeetingsClearFinished)
	s.mux.HandleFunc("DELETE /api/meetings/schedule/{eid}", s.handleMeetingsScheduleDelete)
}

// auditMeetingDeleted records one meeting_delete.
func (s *Server) auditMeetingDeleted(r *http.Request, d MeetingDeleted, via string) {
	s.auditFromRequest(r, "meeting_delete", auditDetail("meeting", d.ID, "state", d.State, "via", via,
		"upstream_error", boolString(d.UpstreamError != ""), "calendar_removed", strconv.Itoa(d.CalendarRemoved)), "")
}

func (s *Server) handleMeetingsDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	d, err := svc.Delete(ctx, r.PathValue("id"))
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	s.auditMeetingDeleted(r, d, "meetings")
	jsonResponse(w, d)
}

func (s *Server) handleMeetingsClearFinished(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	var req meetingsClearRequest
	if r.ContentLength > 0 {
		if err := decodeBody(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	before := time.Now()
	if b := strings.TrimSpace(req.Before); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			jsonError(w, "before must be an RFC 3339 time", http.StatusBadRequest)
			return
		}
		if t.Before(before) {
			before = t
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	list, err := svc.ClearFinished(ctx, before)
	if list == nil {
		list = []MeetingDeleted{}
	}
	for _, d := range list {
		s.auditMeetingDeleted(r, d, "clear_finished")
	}
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"deleted": list, "count": len(list)})
}

func (s *Server) handleMeetingsScheduleDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc, ok := s.meetingsScheduleService(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingsCallTimeout)
	defer cancel()
	eid := r.PathValue("eid")
	d, err := svc.Delete(ctx, eid)
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	if d.Meetings == nil {
		d.Meetings = []MeetingDeleted{}
	}
	for _, m := range d.Meetings {
		s.auditMeetingDeleted(r, m, "calendar")
	}
	s.auditFromRequest(r, "meeting_schedule_delete", auditDetail("event", eid, "meetings", strconv.Itoa(len(d.Meetings))), "")
	jsonResponse(w, d)
}
